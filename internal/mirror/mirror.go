// Package mirror keeps GitCove's copy of every installation repo up to date.
// (Named mirror rather than the plan's sync, which would shadow the standard library package.)
package mirror

import (
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
	// Branches are listed before the fetch, so a branch created in between is fetched but not
	// listed, and counts as protected until the next sync.
	err := errors.Join(m.syncBranches(ctx, r), m.fetch(ctx, r), m.syncIssues(ctx, r))
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
// shortcut: deleted comments and issues stay until the weekly full scan of milestone 7.
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
	return m.Store.SetLabels(ctx, r.ID, labels)
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

// Run calls Cycle every interval until ctx ends.
func (m *Mirror) Run(ctx context.Context, every time.Duration) {
	for {
		start := time.Now()
		if err := m.Cycle(ctx); err != nil {
			m.Log.Warn("sync failed", "err", err)
		} else {
			m.Log.Info("sync done", "took", time.Since(start).Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
