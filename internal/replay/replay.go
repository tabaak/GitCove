// Package replay returns work done on GitCove during failover to GitHub. Pushes (design 8.6 and
// 8.7) never force-push (R1) and never go straight to a default, deploy or protected branch (R4).
// Changes to issues (issues.go) are posted at most once (R3) and never overwrite newer content
// (R5). Replay is safe to stop and resume at any point, because every decision is re-derived
// from the database, the repos and GitHub, and every state change is one transaction (R6).
package replay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/mirror"
	"github.com/tabaak/gitcove/internal/store"
)

type Replayer struct {
	GH     ghclient.Client
	Store  *store.Store
	Git    *gitstore.Store
	Mirror *mirror.Mirror
	Log    *slog.Logger
	Alert  func(msg string)
	Kill   func(point string) error // tests only: fail at a named point, as a crash would

	Users      *auth.Users   // people's GitHub tokens; nil means the App posts everything
	MarkerWait time.Duration // how long after a write its marker must show up in listings; 0 means 15s (spike M)

	paused bool // alert once per pause, not once per pass
}

// Kind is what replay does with a branch.
type Kind string

const (
	Nothing     Kind = "nothing to push"
	Already     Kind = "already on GitHub"
	FastForward Kind = "push"
	PR          Kind = "PR"
	Conflict    Kind = "conflict"
)

// Action is the plan for one branch: every queued push to it is settled by one action.
type Action struct {
	Repo   store.Repo
	Branch string
	Items  []store.RefItem
	Tip    string // the served branch's commit, what replay sends
	GitHub string // GitHub's tip as last fetched, "" if GitHub lacks the branch
	Kind   Kind
	Target string // the GitHub branch pushed to: Branch, or a gitcove/ fallback for a PR
	Reason string
}

// Plan fetches each repo with queued pushes and decides what to do with each branch. Repos that
// can't be fetched are planned from the last copy and reported in errs.
func (r *Replayer) Plan(ctx context.Context) (actions []Action, errs []error, err error) {
	items, err := r.Store.RefItems(ctx, 0, "pending", "sending")
	if err != nil || len(items) == 0 {
		return nil, nil, err
	}
	repos, err := r.Store.Repos(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, repo := range repos {
		var mine []store.RefItem
		for _, it := range items {
			if it.RepoID == repo.ID {
				mine = append(mine, it)
			}
		}
		if len(mine) == 0 {
			continue
		}
		if err := r.Mirror.SyncRepo(ctx, repo); err != nil {
			errs = append(errs, err)
		}
		pol, err := r.Store.Policy(ctx, repo.ID)
		if err != nil {
			return nil, nil, err
		}
		var branches []string
		for _, it := range mine {
			if !slices.Contains(branches, it.Branch) {
				branches = append(branches, it.Branch)
			}
		}
		for _, b := range branches {
			a := Action{Repo: repo, Branch: b}
			for _, it := range mine {
				if it.Branch == b {
					a.Items = append(a.Items, it)
				}
			}
			if err := r.decide(ctx, pol, &a); err != nil {
				return nil, nil, fmt.Errorf("%s %s: %w", repo.FullName(), b, err)
			}
			actions = append(actions, a)
		}
	}
	return actions, errs, nil
}

// decide compares the served branch with GitHub's copy (design 8.6, "comparing commit history").
func (r *Replayer) decide(ctx context.Context, pol store.Policy, a *Action) error {
	id := a.Repo.GitHubID
	var err error
	if a.Tip, err = r.Git.Ref(ctx, id, "refs/heads/"+a.Branch); err != nil {
		return err
	}
	if a.GitHub, err = r.Git.Ref(ctx, id, gitstore.GitHubHeads+a.Branch); err != nil {
		return err
	}
	if a.Tip == "" {
		a.Kind, a.Reason = Nothing, "the branch isn't on GitCove, so the push never completed"
		return nil
	}
	// A fallback branch already chosen, possibly already pushed, is kept, so a restart opens its PR.
	fallback := ""
	for _, it := range a.Items {
		if it.Target != "" && it.Target != a.Branch {
			fallback = it.Target
		}
	}
	on, err := r.Git.GitHubBranchesContaining(ctx, id, a.Tip)
	if err != nil {
		return err
	}
	on = slices.DeleteFunc(on, func(b string) bool { return b == fallback })
	onGitHub := a.GitHub != ""
	switch {
	case len(on) > 0:
		a.Kind, a.Reason = Already, "GitHub has these commits on "+strings.Join(on, ", ")
		return nil
	case pol.PROnly(a.Branch, onGitHub) && !onGitHub:
		a.Kind, a.Reason = Conflict, "a default, deploy or protected branch that GitHub doesn't have, so there's nothing to open a PR into"
		return nil
	case pol.PROnly(a.Branch, onGitHub):
		a.Kind, a.Reason = PR, "default, deploy or protected branch: reaches GitHub only through a PR"
	case fallback != "":
		a.Kind, a.Reason = PR, "resuming the PR already started"
	default:
		ff := !onGitHub
		if onGitHub {
			if ff, err = r.Git.IsAncestor(ctx, id, a.GitHub, a.Tip); err != nil {
				return err
			}
		}
		a.Kind, a.Target, a.Reason = FastForward, a.Branch, "fast-forward"
		if !onGitHub {
			a.Reason = "new branch"
		}
		if !ff {
			a.Kind, a.Target, a.Reason = PR, "", "diverged from GitHub"
		}
	}
	if a.Kind == PR {
		a.Target = cmp.Or(fallback, fmt.Sprintf("gitcove/%s-%s", a.Branch, time.Now().UTC().Format("20060102-150405")))
	}
	ok, err := r.Git.WorkflowsMatchGitHub(ctx, id, a.Tip)
	if err != nil {
		return err
	}
	if !ok {
		a.Kind, a.Target = Conflict, ""
		a.Reason = "its .github/workflows differs from every branch on GitHub, so GitHub would refuse the push. " +
			"Merge the current target branch into it and push again"
	}
	return nil
}

func (r *Replayer) kill(point string) error {
	if r.Kill != nil {
		return r.Kill(point)
	}
	return nil
}

func ids(items []store.RefItem) []int64 {
	var out []int64
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// errStopped means the mode left Replaying, e.g. because failover started again.
var errStopped = errors.New("replay stopped: no longer replaying")

// Execute carries out one action. An error means replay should pause; the items keep their state.
func (r *Replayer) Execute(ctx context.Context, a Action) error {
	switch a.Kind {
	case Nothing, Already:
		return r.Store.Finish(ctx, ids(a.Items), "done", string(a.Kind), "", "")
	case Conflict:
		return r.Store.Finish(ctx, ids(a.Items), "conflict", a.Reason, a.Tip, a.GitHub)
	}
	if err := r.Store.MarkSending(ctx, ids(a.Items), a.Target); errors.Is(err, store.ErrWrongMode) {
		return errStopped
	} else if err != nil {
		return err
	}
	if err := r.kill("sending"); err != nil {
		return err
	}
	tok, err := r.GH.GitToken(ctx)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, r.Mirror.FetchTimeout)
	err = r.Git.Push(pctx, a.Repo.GitHubID, a.Repo.Owner, a.Repo.Name, tok, a.Tip, a.Target)
	cancel()
	var rej *gitstore.RejectedError
	switch {
	case errors.As(err, &rej) && rej.NonFastForward && a.Kind == FastForward:
		return nil // GitHub moved since the fetch; the next pass fetches and decides again
	case errors.As(err, &rej):
		return r.Store.Finish(ctx, ids(a.Items), "conflict", "GitHub refused the push to "+a.Target+": "+rej.Reason, a.Tip, a.GitHub)
	case err != nil:
		return err // may have landed: the next pass sees it on GitHub
	}
	if err := r.kill("pushed"); err != nil {
		return err
	}
	if a.Kind == FastForward {
		return r.Store.Finish(ctx, ids(a.Items), "done", "fast-forward", "", "")
	}
	n, err := r.openPR(ctx, a)
	if err != nil {
		return err
	}
	if n == 0 {
		return r.Store.Finish(ctx, ids(a.Items), "conflict", "GitHub refused the PR from "+a.Target+
			", and none from that branch was found. Check GitHub, then skip or retry this item", a.Tip, a.GitHub)
	}
	if err := r.kill("pr"); err != nil {
		return err
	}
	return r.Store.Finish(ctx, ids(a.Items), "done", fmt.Sprintf("PR #%d from %s", n, a.Target), "", "")
}

// openPR opens the PR from the fallback branch at most once. The branch name is fixed before
// anything is sent and unique to this work, so a PR from it, found in any state, is this one;
// and GitHub itself refuses a second open PR with the same head and base. It returns 0 if
// GitHub refused the PR and none exists.
func (r *Replayer) openPR(ctx context.Context, a Action) (int, error) {
	if n, err := r.GH.FindPR(ctx, a.Repo.Owner, a.Repo.Name, a.Target); err != nil || n != 0 {
		return n, err
	}
	n, err := r.GH.CreatePR(ctx, a.Repo.Owner, a.Repo.Name, "", ghclient.NewPR{
		Title: "GitCove: work pushed to " + a.Branch + " during a GitHub outage",
		Head:  a.Target, Base: a.Branch, Body: prBody(a),
	})
	var se *ghclient.StatusError
	if errors.As(err, &se) && se.Status == 422 {
		return r.GH.FindPR(ctx, a.Repo.Owner, a.Repo.Name, a.Target)
	}
	return n, err
}

func prBody(a Action) string {
	var pushers []string
	for _, it := range a.Items {
		if p := "`" + it.Pusher + "`"; !slices.Contains(pushers, p) {
			pushers = append(pushers, p) // backticks, so nobody is notified
		}
	}
	return fmt.Sprintf("Pushed to `%s` on GitCove during a GitHub outage by %s, and replayed here as a PR because: %s.\n\n"+
		"Commit `%.12s`. Merging is up to you and this repo's usual checks; GitCove never merges.\n\n"+
		"<!-- gitcove:op=%s -->\n", a.Branch, strings.Join(pushers, ", "), a.Reason, a.Tip, a.Items[0].OpID)
}

// Pass replays everything queued, if the instance is replaying. On an error (GitHub failing
// again, say) it alerts and returns; items keep their state and the next pass resumes them.
// Once nothing is left it returns the instance to normal.
func (r *Replayer) Pass(ctx context.Context) error {
	if m, err := r.Store.Mode(ctx); err != nil || m != store.Replaying {
		return err
	}
	err := r.pass(ctx)
	if errors.Is(err, errStopped) {
		return nil
	}
	if err != nil {
		if !r.paused && r.Alert != nil {
			r.Alert("replay paused, will retry: " + err.Error())
		}
		r.paused = true
		return err
	}
	r.paused = false
	if err := r.Store.SetMode(ctx, "replay", store.Normal); err != nil && !errors.Is(err, store.ErrWrongMode) {
		return err
	}
	return nil
}

func (r *Replayer) pass(ctx context.Context) error {
	actions, fetchErrs, err := r.Plan(ctx)
	if err != nil {
		return err
	}
	if len(fetchErrs) > 0 {
		// Don't push on a stale view of GitHub; also, GitHub is probably failing again.
		return errors.Join(fetchErrs...)
	}
	for _, a := range actions {
		if err := r.Execute(ctx, a); err != nil {
			return fmt.Errorf("%s %s: %w", a.Repo.FullName(), a.Branch, err)
		}
	}
	return r.replayIssues(ctx) // after the refs (design 8.5)
}

// Run calls Pass every interval until ctx ends.
func (r *Replayer) Run(ctx context.Context, every time.Duration) {
	for {
		if err := r.Pass(ctx); err != nil {
			r.Log.Warn("replay pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
