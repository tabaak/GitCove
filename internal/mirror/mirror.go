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

// Cycle lists the installation's repos and fetches each one. A repo that fails is retried next cycle.
func (m *Mirror) Cycle(ctx context.Context) error {
	repos, err := m.GH.Repos(ctx)
	if err != nil {
		return fmt.Errorf("list repos: %w", err)
	}
	var errs []error
	for _, r := range repos {
		full := r.Owner.Login + "/" + r.Name
		id, err := m.Store.UpsertRepo(ctx, r.ID, r.Owner.Login, r.Name, r.DefaultBranch)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, err))
			continue
		}
		err = m.fetch(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", full, err))
		}
		if err := m.Store.MarkSynced(ctx, id, err); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

func (m *Mirror) fetch(ctx context.Context, r ghclient.Repo) error {
	tok, err := m.GH.GitToken(ctx) // cached, so a long cycle still gets a fresh token
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.FetchTimeout)
	defer cancel()
	return m.Git.Fetch(ctx, r.ID, r.Owner.Login, r.Name, r.DefaultBranch, tok)
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
