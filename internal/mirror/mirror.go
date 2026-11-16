// Package mirror keeps GitCove's copy of every installation repo up to date.
// (Named mirror rather than the plan's sync, which would shadow the standard library package.)
package mirror

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/store"
)

type Mirror struct {
	GH           ghclient.Client
	Store        *store.Store
	Git          *gitstore.Store
	FetchTimeout time.Duration
	Log          *slog.Logger
	Wake         chan struct{} // a webhook queued a sync; buffered, size 1
}

// Cycle lists the installation's repos and syncs each one. A repo that fails is retried next cycle.
func (m *Mirror) Cycle(ctx context.Context) error {
	repos, err := m.GH.Repos(ctx)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	var errs []error
	for _, r := range repos {
		sr := store.Repo{GitHubID: r.ID, Owner: r.Owner.Login, Name: r.Name, DefaultBranch: r.DefaultBranch}
		if sr.ID, err = m.Store.UpsertRepo(ctx, r.ID, sr.Owner, sr.Name, sr.DefaultBranch); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sr.FullName(), err))
			continue
		}
		if err := m.SyncRepo(ctx, sr); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SyncRepo refreshes one repo's branch protection, fetches it, and syncs its issues, comments
// and labels, then records the outcome.
func (m *Mirror) SyncRepo(ctx context.Context, r store.Repo) error {
	if err := m.Store.ClearWanted(ctx, r.ID); err != nil {
		return err
	}
	// Branches are listed before the fetch, so a branch created in between is fetched but not
	// listed, and counts as protected until the next sync.
	err := errors.Join(m.syncBranches(ctx, r), m.fetch(ctx, r), m.syncIssues(ctx, r))
	if err == nil {
		err = m.fullScan(ctx, r)
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", r.FullName(), err)
	}
	if serr := m.Store.MarkSynced(ctx, r.ID, err); serr != nil {
		return serr
	}
	return err
}

func (m *Mirror) syncBranches(ctx context.Context, r store.Repo) error {
	branches, err := m.GH.Branches(ctx, r.Owner, r.Name)
	if err != nil {
		return fmt.Errorf("list branches: %w", err)
	}
	prot := map[string]bool{}
	for _, b := range branches {
		prot[b.Name] = b.Protected
	}
	return m.Store.SetBranches(ctx, r.ID, prot)
}

// syncIssues backfills, then polls, issues, PRs and comments, oldest update first, saving a
// cursor with each page so a backfill resumes where it stopped (design 7). The listing's since
// stays fixed until something changes, so an idle repo costs two 304s and a label listing.
// Deletions are invisible here: fullScan finds them.
func (m *Mirror) syncIssues(ctx context.Context, r store.Repo) error {
	issuesCur, commentsCur := fmt.Sprintf("repo:%d:issues", r.ID), fmt.Sprintf("repo:%d:comments", r.ID)
	since, err := m.Store.Cursor(ctx, issuesCur)
	if err != nil {
		return err
	}
	err = m.GH.IssuesSince(ctx, r.Owner, r.Name, back(since), func(page []ghclient.Issue) error {
		var out []store.Issue
		for _, i := range page {
			si := store.Issue{Number: i.Number, IsPR: i.PullRequest != nil, Title: i.Title, Body: i.Body, State: i.State,
				Author: i.User.Login, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt}
			for _, l := range i.Labels {
				si.Labels = append(si.Labels, l.Name)
			}
			out = append(out, si)
			since = max(since, i.UpdatedAt)
		}
		return m.Store.SaveIssues(ctx, r.ID, issuesCur, since, out)
	})
	if err != nil {
		return fmt.Errorf("sync issues: %w", err)
	}
	if since, err = m.Store.Cursor(ctx, commentsCur); err != nil {
		return err
	}
	err = m.GH.CommentsSince(ctx, r.Owner, r.Name, back(since), func(page []ghclient.Comment) error {
		var out []store.Comment
		for _, c := range page {
			n, err := strconv.Atoi(c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:])
			if err != nil {
				return fmt.Errorf("comment %d: issue_url %q", c.ID, c.IssueURL)
			}
			out = append(out, store.Comment{ID: c.ID, Number: n, Author: c.User.Login, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
			since = max(since, c.UpdatedAt)
		}
		return m.Store.SaveComments(ctx, r.ID, commentsCur, since, out)
	})
	if err != nil {
		return fmt.Errorf("sync comments: %w", err)
	}
	labels, err := m.GH.Labels(ctx, r.Owner, r.Name)
	if err != nil {
		return fmt.Errorf("sync labels: %w", err)
	}
	if err := m.Store.SetLabels(ctx, r.ID, labels); err != nil {
		return err
	}
	return m.syncPulls(ctx, r)
}

// syncPulls mirrors what PRs have beyond their issue row: branches, merged or not, reviews and
// line comments. Reviews have no repo-wide listing, so they are read per PR, only for open PRs
// that changed since the last sync.
func (m *Mirror) syncPulls(ctx context.Context, r store.Repo) error {
	pullsCur, commentsCur := fmt.Sprintf("repo:%d:pulls", r.ID), fmt.Sprintf("repo:%d:review_comments", r.ID)
	since, err := m.Store.Cursor(ctx, pullsCur)
	if err != nil {
		return err
	}
	newest := since
	var open []int
	err = m.GH.PullsSince(ctx, r.Owner, r.Name, back(since), func(page []ghclient.PullRequest) error {
		var out []store.PullRequest
		for _, p := range page {
			out = append(out, store.PullRequest{Number: p.Number, HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Fork: p.Fork(),
				Merged: p.MergedAt != nil, UpdatedAt: p.UpdatedAt})
			if p.State == "open" {
				open = append(open, p.Number)
			}
			newest = max(newest, p.UpdatedAt)
		}
		return m.Store.SavePulls(ctx, r.ID, out)
	})
	if err != nil {
		return fmt.Errorf("sync pull requests: %w", err)
	}
	for _, n := range open {
		reviews, err := m.GH.Reviews(ctx, r.Owner, r.Name, n)
		if err != nil {
			return fmt.Errorf("sync reviews of #%d: %w", n, err)
		}
		var out []store.Review
		for _, v := range reviews {
			out = append(out, store.Review{ID: v.ID, Number: n, Author: v.User.Login, State: v.State, Body: v.Body, Commit: v.CommitID, SubmittedAt: v.SubmittedAt})
		}
		if err := m.Store.SetReviews(ctx, r.ID, n, out); err != nil {
			return err
		}
	}
	// The listing is newest first, so the cursor moves only once everything newer is saved.
	if newest != since {
		if err := m.Store.SetCursor(ctx, pullsCur, newest); err != nil {
			return err
		}
	}
	if since, err = m.Store.Cursor(ctx, commentsCur); err != nil {
		return err
	}
	err = m.GH.ReviewCommentsSince(ctx, r.Owner, r.Name, back(since), func(page []ghclient.ReviewComment) error {
		var out []store.ReviewComment
		for _, c := range page {
			n, err := strconv.Atoi(c.PullRequestURL[strings.LastIndex(c.PullRequestURL, "/")+1:])
			if err != nil {
				return fmt.Errorf("line comment %d: pull_request_url %q", c.ID, c.PullRequestURL)
			}
			line := 0
			if l := cmp.Or(c.Line, c.OriginalLine); l != nil {
				line = *l
			}
			out = append(out, store.ReviewComment{ID: c.ID, InReplyTo: c.InReplyTo, Number: n, Author: c.User.Login, Path: c.Path, Line: line,
				Side: c.Side, Commit: c.OriginalCommitID, Body: c.Body, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
			since = max(since, c.UpdatedAt)
		}
		return m.Store.SaveReviewComments(ctx, r.ID, commentsCur, since, out)
	})
	if err != nil {
		return fmt.Errorf("sync line comments: %w", err)
	}
	return nil
}

// back moves a cursor a minute earlier. GitHub's listings can lag behind a write by seconds
// (spike M), so an item updated just before the newest one seen may only be listed after it.
// The since value still changes only when the cursor does, so an idle repo still gets a 304.
func back(cursor string) string {
	t, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return ""
	}
	return t.Add(-time.Minute).UTC().Format(time.RFC3339)
}

func (m *Mirror) fetch(ctx context.Context, r store.Repo) error {
	tok, err := m.GH.GitToken(ctx) // cached, so a long cycle still gets a fresh token
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.FetchTimeout)
	defer cancel()
	return m.Git.Fetch(ctx, r.GitHubID, r.Owner, r.Name, r.DefaultBranch, tok, func() (map[string]bool, error) {
		return m.Store.HeldBranches(ctx, r.ID)
	})
}

// ScanEvery is how often fullScan runs without a webhook reporting a deletion.
const ScanEvery = 7 * 24 * time.Hour

// fullScan lists every issue, comment and line comment, weekly or after a webhook reports a
// deletion, and moves what GitHub no longer has to tombstones. A listing can skip an item when
// others change while it's paged, so each missing item is checked with GitHub before burial.
func (m *Mirror) fullScan(ctx context.Context, r store.Repo) error {
	cur := store.FullScanCursor(r.ID)
	last, err := m.Store.Cursor(ctx, cur)
	if err != nil {
		return err
	}
	started := time.Now().UTC().Format(time.RFC3339)
	if last == "" { // the first sync's backfill just listed everything
		return m.Store.SetCursor(ctx, cur, started)
	}
	if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < ScanEvery {
		return nil // last is store.ScanDue after a deletion webhook, which doesn't parse
	}
	seen := map[string]map[int64]bool{"issue": {}, "comment": {}, "review_comment": {}}
	err = errors.Join(
		m.GH.IssuesSince(ctx, r.Owner, r.Name, "", func(p []ghclient.Issue) error {
			for _, i := range p {
				seen["issue"][int64(i.Number)] = true
			}
			return nil
		}),
		m.GH.CommentsSince(ctx, r.Owner, r.Name, "", func(p []ghclient.Comment) error {
			for _, c := range p {
				seen["comment"][c.ID] = true
			}
			return nil
		}),
		m.GH.ReviewCommentsSince(ctx, r.Owner, r.Name, "", func(p []ghclient.ReviewComment) error {
			for _, c := range p {
				seen["review_comment"][c.ID] = true
			}
			return nil
		}))
	if err != nil {
		return fmt.Errorf("full scan: %w", err)
	}
	for kind, listed := range seen {
		known, err := m.Store.Known(ctx, r.ID, kind)
		if err != nil {
			return err
		}
		missing := map[int][]int64{} // by issue or PR number
		for id, n := range known {
			if !listed[id] {
				missing[n] = append(missing[n], id)
			}
		}
		var gone []int64
		for n, ids := range missing {
			still, err := m.stillThere(ctx, r, kind, n)
			if err != nil {
				return fmt.Errorf("full scan: check %s on #%d: %w", kind, n, err)
			}
			for _, id := range ids {
				if !still[id] {
					gone = append(gone, id)
				}
			}
		}
		if err := m.Store.Bury(ctx, r.ID, kind, gone, m.Git.Keep); err != nil {
			return err
		}
	}
	return m.Store.SetCursor(ctx, cur, started)
}

// stillThere asks GitHub which items of kind on issue or PR n exist, by ID (by number for an issue).
func (m *Mirror) stillThere(ctx context.Context, r store.Repo, kind string, n int) (map[int64]bool, error) {
	ids := map[int64]bool{}
	var err error
	switch kind {
	case "issue":
		_, err = m.GH.Issue(ctx, r.Owner, r.Name, n)
		ids[int64(n)] = err == nil
	case "comment":
		var cs []ghclient.Comment
		cs, err = m.GH.IssueComments(ctx, r.Owner, r.Name, n, "")
		for _, c := range cs {
			ids[c.ID] = true
		}
	case "review_comment":
		var cs []ghclient.ReviewComment
		cs, err = m.GH.ReviewComments(ctx, r.Owner, r.Name, n, 0, "")
		for _, c := range cs {
			ids[c.ID] = true
		}
	}
	if se := (*ghclient.StatusError)(nil); errors.As(err, &se) && (se.Status == 404 || se.Status == 410) {
		return ids, nil // the issue itself is gone, and with it everything on it
	}
	return ids, err
}

// Run calls Cycle every interval until ctx ends, and between cycles syncs the repos webhooks ask for.
func (m *Mirror) Run(ctx context.Context, every time.Duration) {
	var due <-chan time.Time // a full interval after the last full cycle ended
	for full := true; ; {
		start := time.Now()
		var err error
		if full {
			err = m.Cycle(ctx)
		} else {
			err = m.SyncWanted(ctx)
		}
		if err != nil {
			m.Log.Warn("sync failed", "err", err)
		} else {
			m.Log.Info("sync done", "full", full, "took", time.Since(start).Round(time.Millisecond))
		}
		if full {
			due = time.After(every)
		}
		select {
		case <-ctx.Done():
			return
		case <-due:
			full = true
		case <-m.Wake:
			full = false
		}
	}
}

// SyncWanted syncs the repos webhooks asked for. A webhook is only a hint: the sync fetches
// GitHub's current state, so dropped, repeated or reordered deliveries can't make it wrong.
func (m *Mirror) SyncWanted(ctx context.Context) error {
	repos, err := m.Store.WantedRepos(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range repos {
		errs = append(errs, m.SyncRepo(ctx, r))
	}
	return errors.Join(errs...)
}
