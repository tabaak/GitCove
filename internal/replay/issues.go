package replay

// Replay of changes to issues and PRs (design 8.2 to 8.6, 8.10 and 8.12): new issues, comments,
// labels, open or closed state, and title and description edits. Text is posted with a hidden
// marker and the item marked sending first, so a write whose answer was lost is found on GitHub
// rather than posted again (R3). One that can't be found becomes uncertain, never a repost.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/store"
)

// localRef is a reference to a new issue in text, rewritten to its GitHub number when posted.
var localRef = regexp.MustCompile(`#(L-\d+)\b`)

// locals returns the GitHub numbers of the new issues an item depends on: the one it acts on and
// any #L-n in its text. If one has no number yet, it returns why the item waits instead (design 8.5).
func (r *Replayer) locals(ctx context.Context, it store.Item) (map[string]int, string, error) {
	var refs []string
	if it.Kind != "issue" && strings.HasPrefix(it.Issue, "L-") {
		refs = append(refs, it.Issue)
	}
	for _, text := range []string{it.Change.Title, it.Change.Body, it.Change.Value, it.Base} {
		for _, m := range localRef.FindAllStringSubmatch(text, -1) {
			if m[1] != it.Issue {
				refs = append(refs, m[1])
			}
		}
	}
	nums := map[string]int{}
	for _, l := range refs {
		if _, ok := nums[l]; ok {
			continue
		}
		n, state, err := r.Store.MappedIssue(ctx, it.RepoID, l)
		switch {
		case err != nil:
			return nil, "", err
		case state == "": // text mentioning an L-n that never existed: left as written
		case n == 0 && it.Kind == "issue":
			// A new issue never waits for another, or two that mention each other would wait
			// forever: a mention of one not on GitHub yet stays as written.
		case n == 0:
			return nil, fmt.Sprintf("waits for %s, which is %s", l, state), nil
		default:
			nums[l] = n
		}
	}
	return nums, "", nil
}

func rewrite(text string, nums map[string]int) string {
	return localRef.ReplaceAllStringFunc(text, func(m string) string {
		if n, ok := nums[m[1:]]; ok {
			return "#" + strconv.Itoa(n)
		}
		return m
	})
}

func number(it store.Item, nums map[string]int) int {
	if n, ok := nums[it.Issue]; ok {
		return n
	}
	n, _ := strconv.Atoi(it.Issue)
	return n
}

// Step is one queued change to an issue, for the plan: Waits says why it can't replay yet.
type Step struct {
	store.Item
	Waits string
}

// IssuePlan lists the queued changes to issues in replay order.
func (r *Replayer) IssuePlan(ctx context.Context) ([]Step, error) {
	items, err := r.Store.MetaItems(ctx, 0, "pending", "sending")
	var steps []Step
	for _, it := range items {
		_, why, err := r.locals(ctx, it)
		if err != nil {
			return nil, err
		}
		steps = append(steps, Step{it, why})
	}
	return steps, err
}

// replayIssues replays every queued change to issues that isn't waiting for another.
func (r *Replayer) replayIssues(ctx context.Context) error {
	items, err := r.Store.MetaItems(ctx, 0, "pending", "sending")
	if err != nil || len(items) == 0 {
		return err
	}
	repos, err := r.Store.Repos(ctx)
	if err != nil {
		return err
	}
	byID := map[int64]store.Repo{}
	for _, repo := range repos {
		byID[repo.ID] = repo
	}
	for _, it := range items {
		nums, why, err := r.locals(ctx, it)
		if err != nil {
			return err
		}
		if why != "" {
			continue
		}
		if err := r.replayItem(ctx, byID[it.RepoID], it, nums); err != nil {
			return fmt.Errorf("%s item %d (%s on %s): %w", it.Repo, it.ID, it.Kind, it.Issue, err)
		}
	}
	return nil
}

func (r *Replayer) replayItem(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int) error {
	if it.State == "sending" && it.Target != "apply" {
		// A text write that may already be on GitHub: look for it, never resend (design 8.3).
		sent, err := time.Parse(time.RFC3339Nano, it.Updated)
		if err != nil {
			return err
		}
		return r.recoverItem(ctx, repo, it, nums, sent)
	}
	action := "post"
	switch it.Kind {
	case "label":
		action = "apply" // one change, never the whole set; repeating it is harmless
	case "state", "edit":
		// Decided against GitHub's current version, not the mirror's (R5). Setting a value is
		// harmless to repeat, so an apply interrupted by a crash is simply decided again.
		gh, err := r.GH.Issue(ctx, repo.Owner, repo.Name, number(it, nums))
		if refused(err) {
			return r.Store.Finish(ctx, []int64{it.ID}, "conflict", "GitHub can't show the issue: "+err.Error(), local(it), "")
		} else if err != nil {
			return err
		}
		cur, want, base := gh.State, it.Change.State, it.Base
		if it.Kind == "edit" {
			cur, want, base = gh.Title, rewrite(it.Change.Value, nums), rewrite(it.Base, nums)
			if it.Change.Field == "body" {
				cur = gh.Body
			}
		}
		switch cur {
		case want:
			return r.Store.Finish(ctx, []int64{it.ID}, "done", "already so on GitHub", "", "")
		case base:
			action = "apply"
		default:
			action = "note" // GitHub changed it too: keep GitHub's, and say so in a comment (design 8.6)
		}
	}
	poster, tok, err := r.Users.Token(ctx, it.TokenID)
	if err != nil {
		return err
	}
	return r.send(ctx, repo, it, nums, action, poster, tok)
}

func refused(err error) bool {
	var se *ghclient.StatusError
	return errors.As(err, &se) && se.Status/100 == 4
}

// send marks the item sending, then writes it to GitHub as poster (the App if "").
func (r *Replayer) send(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, action, poster, tok string) error {
	if err := r.Store.MarkMeta(ctx, it.ID, action, poster); errors.Is(err, store.ErrWrongMode) {
		return errStopped
	} else if err != nil {
		return err
	}
	it.State, it.Target, it.Poster = "sending", action, poster
	sent := time.Now()
	if err := r.kill("sending"); err != nil {
		return err
	}
	n, id, err := r.write(ctx, repo, it, nums, tok)
	var se *ghclient.StatusError
	errors.As(err, &se)
	switch {
	case err == nil:
	case errors.Is(err, ghclient.ErrNotSent), ghclient.IsRateLimited(err):
		// Never sent (e.g. stopped while waiting for the write budget), or refused for a rate limit
		// without being carried out; the client then holds every request until the block lifts.
		return errors.Join(err, r.Store.Requeue(context.WithoutCancel(ctx), it.ID))
	case refused(err) && poster != "" && (se.Status == 401 || se.Status == 403):
		// The person's token no longer works here: the App posts instead (design 8.12).
		r.Log.Warn("user token refused; posting as the App", "item", it.ID, "login", poster, "err", err)
		if se.Status == 401 {
			if err := r.Users.Drop(ctx, it.TokenID, "GitHub refused it: "+err.Error()); err != nil {
				return err
			}
		}
		return r.send(ctx, repo, it, nums, action, "", "")
	case refused(err) && se.Status == 404 && it.Kind == "label" && !it.Change.Add:
		// The label is already off.
	case refused(err):
		return r.Store.Finish(ctx, []int64{it.ID}, "conflict", "GitHub refused it: "+err.Error(), local(it), "")
	case action == "apply":
		return err // harmless to repeat: the next pass decides again
	default:
		return r.recoverItem(ctx, repo, it, nums, sent) // may have been posted
	}
	if err := r.kill("sent"); err != nil {
		return err
	}
	return r.finish(ctx, it, n, id)
}

func (r *Replayer) finish(ctx context.Context, it store.Item, n int, id int64) error {
	if it.Kind == "issue" {
		return r.Store.FinishIssue(ctx, it, n)
	}
	result := "applied"
	switch {
	case it.Target == "note":
		result = fmt.Sprintf("GitHub had changed it too, so GitHub's was kept and a note posted (comment %d)", id)
	case it.Kind == "comment":
		result = fmt.Sprintf("comment %d", id)
	}
	if it.Poster != "" {
		result += " as " + it.Poster
	}
	return r.Store.Finish(ctx, []int64{it.ID}, "done", result, "", "")
}

// write makes the item's one request to GitHub. It returns the new issue's number or comment's ID.
func (r *Replayer) write(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, tok string) (int, int64, error) {
	o, name, n, c := repo.Owner, repo.Name, number(it, nums), it.Change
	switch {
	case it.Kind == "issue":
		num, err := r.GH.CreateIssue(ctx, o, name, tok, ghclient.NewIssue{Title: rewrite(c.Title, nums), Body: text(it, c.Body, nums, tok != ""), Labels: c.Labels})
		return num, 0, err
	case it.Kind == "comment":
		id, err := r.GH.CreateComment(ctx, o, name, n, tok, text(it, c.Body, nums, tok != ""))
		return 0, id, err
	case it.Kind == "label" && c.Add:
		return 0, 0, r.GH.AddLabel(ctx, o, name, n, tok, c.Label)
	case it.Kind == "label":
		return 0, 0, r.GH.RemoveLabel(ctx, o, name, n, tok, c.Label)
	case it.Target == "apply" && it.Kind == "state":
		return 0, 0, r.GH.EditIssue(ctx, o, name, n, tok, map[string]string{"state": c.State})
	case it.Target == "apply":
		return 0, 0, r.GH.EditIssue(ctx, o, name, n, tok, map[string]string{c.Field: rewrite(c.Value, nums)})
	}
	id, err := r.GH.CreateComment(ctx, o, name, n, tok, note(it, nums))
	return 0, id, err
}

func marker(opID string) string { return "<!-- gitcove:op=" + opID + " -->" }

func when(it store.Item) string {
	t, _ := time.Parse(time.RFC3339Nano, it.Created)
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// who names the person in backticks, so nobody is notified (design 5).
func who(it store.Item) string { return "`" + strings.ReplaceAll(it.Change.Author, "`", "") + "`" }

// text is what's posted for a new issue or comment: by the App with an attribution line
// (design 8.10), or as the person with the original time, which GitHub can't set (8.12).
func text(it store.Item, body string, nums map[string]int, asUser bool) string {
	body = rewrite(body, nums)
	if asUser {
		body += "\n\n<sub>Written on GitCove at " + when(it) + " during a GitHub outage.</sub>"
	} else {
		body = "Originally by " + who(it) + " at " + when(it) + " during a GitHub outage.\n\n" + body
	}
	return body + "\n\n" + marker(it.OpID)
}

// note is the comment posted when GitHub changed what the item changes (design 8.6). A state has
// two values, so GitHub changing it usually means it already has the one asked for; a note is
// left only when the change asked for the state GitCove showed, and GitHub moved away from it.
func note(it store.Item, nums map[string]int) string {
	if it.Kind == "state" {
		verb, kept := "closed", "open"
		if it.Change.State == "open" {
			verb, kept = "reopened", "closed"
		}
		return fmt.Sprintf("%s %s this on GitCove at %s during a GitHub outage, but its state changed on GitHub meanwhile, "+
			"so it stays %s.\n\n%s", who(it), verb, when(it), kept, marker(it.OpID))
	}
	field := map[string]string{"title": "title", "body": "description"}[it.Change.Field]
	return fmt.Sprintf("%s proposed a new %s on GitCove at %s during a GitHub outage, but it changed on GitHub meanwhile, "+
		"so GitHub's was kept. The proposal:\n\n> %s\n\n%s", who(it), field, when(it),
		strings.ReplaceAll(rewrite(it.Change.Value, nums), "\n", "\n> "), marker(it.OpID))
}

// local is the item's own version, kept with a conflict.
func local(it store.Item) string {
	b, _ := json.Marshal(it.Change)
	return string(b)
}

// recoverItem looks for a text write's marker on GitHub, waiting at least MarkerWait after it
// was sent and checking three times, because GitHub's listings can lag behind a write (spike M,
// case 6). Found, the item is done. Not found, it is uncertain: a crash before the request went
// out looks the same as a lagging listing, so only a human may post it again.
func (r *Replayer) recoverItem(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, sent time.Time) error {
	wait := r.MarkerWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	for i := range 3 {
		if err := sleepUntil(ctx, sent.Add(wait+time.Duration(i)*wait/2)); err != nil {
			return err
		}
		n, id, err := r.findMarker(ctx, repo, it, nums, sent)
		if err != nil {
			return err
		}
		if n != 0 || id != 0 {
			return r.finish(ctx, it, n, id)
		}
	}
	return r.Store.Finish(ctx, []int64{it.ID}, "uncertain", fmt.Sprintf("GitHub didn't confirm it, and its marker wasn't found "+
		"in 3 checks over %s, so it may or may not be on GitHub. Look there, then skip it if it is, or retry to post it again", 2*wait), local(it), "")
}

// findMarker looks for the item's marker with filtered listings, never search (design 8.4).
func (r *Replayer) findMarker(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, sent time.Time) (int, int64, error) {
	since := sent.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	mark := marker(it.OpID)
	if it.Kind == "issue" {
		creator := it.Poster
		if creator == "" {
			var err error
			if creator, err = r.GH.AppLogin(ctx); err != nil {
				return 0, 0, err
			}
		}
		issues, err := r.GH.IssuesBy(ctx, repo.Owner, repo.Name, creator, since)
		for _, i := range issues {
			if strings.Contains(i.Body, mark) {
				return i.Number, 0, nil
			}
		}
		return 0, 0, err
	}
	comments, err := r.GH.IssueComments(ctx, repo.Owner, repo.Name, number(it, nums), since)
	for _, c := range comments {
		if strings.Contains(c.Body, mark) {
			return 0, c.ID, nil
		}
	}
	return 0, 0, err
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
