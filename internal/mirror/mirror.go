// Package mirror keeps GitCove's copy of every installation repo up to date.
// (Named mirror rather than the plan's sync, which would shadow the standard library package.)
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// SyncRepo refreshes one repo's branch protection and fetches it, then records the outcome.
func (m *Mirror) SyncRepo(ctx context.Context, r store.Repo) error {
	// Branches are listed before the fetch, so a branch created in between is fetched but not
	// listed, and counts as protected until the next sync.
	err := errors.Join(m.syncBranches(ctx, r), m.fetch(ctx, r))
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
