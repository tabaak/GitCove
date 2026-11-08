package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestModesAndPolicy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cove.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := s.UpsertRepo(ctx, 1, "org", "app", "main")
	up := []RefUpdate{{"fix", "", "abc"}}

	if err := s.RecordPush(ctx, repo, "ben", up); !errors.Is(err, ErrWrongMode) {
		t.Fatalf("push recorded outside failover: %v", err)
	}
	if err := s.SetMode(ctx, "t", Reconciling); !errors.Is(err, ErrWrongMode) {
		t.Fatal("normal -> reconciling allowed")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetMode(ctx, "t", Failover))
	must(s.AllowProtected(ctx, "t", true))
	must(s.RecordPush(ctx, repo, "ben", up))
	must(s.SetMode(ctx, "t", Reconciling))
	if p, _ := s.Policy(ctx, repo); p.AllowProtected {
		t.Fatal("emergency permission outlived the failover")
	}
	must(s.SetMode(ctx, "t", Replaying))
	if err := s.SetMode(ctx, "t", Normal); !errors.Is(err, ErrWrongMode) {
		t.Fatal("normal with an item still pending")
	}
	items, _ := s.RefItems(ctx, 0, "pending")
	if len(items) != 1 || items[0].Branch != "fix" || items[0].Pusher != "ben" || items[0].New != "abc" {
		t.Fatalf("items = %+v", items)
	}
	must(s.MarkSending(ctx, []int64{items[0].ID}, "fix"))
	must(s.Finish(ctx, []int64{items[0].ID}, "conflict", "refused", "abc", "def"))
	if held, _ := s.HeldBranches(ctx, repo); !held["fix"] {
		t.Fatal("conflicted branch not held")
	}
	must(s.SetMode(ctx, "t", Normal))

	// Survives a restart.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, _ := s.Mode(ctx); m != Normal {
		t.Fatalf("mode after reopen = %s", m)
	}
	must(s.Resolve(ctx, "t", items[0].ID, false))
	if held, _ := s.HeldBranches(ctx, repo); held["fix"] {
		t.Fatal("skipped branch still held")
	}
	log, _ := s.Audit(ctx)
	if len(log) != 6 {
		t.Fatalf("audit log has %d entries, want 6: %+v", len(log), log)
	}

	// Protection: unknown counts as protected for branches GitHub has; new branches are GitHub's call.
	p, _ := s.Policy(ctx, repo)
	if !p.IsProtected("main", true) || p.IsProtected("new", false) {
		t.Fatal("protection before the first branch sync")
	}
	must(s.SetBranches(ctx, repo, map[string]bool{"main": false, "release": true}))
	r, _ := s.RepoByName(ctx, "org", "app")
	must(s.MarkBranch(ctx, "t", r, "hotfix", "protected", true))
	must(s.MarkBranch(ctx, "t", r, "prod", "deploy", true))
	p, _ = s.Policy(ctx, repo)
	for _, c := range []struct {
		branch            string
		onGitHub          bool
		protected, prOnly bool
	}{
		{"main", true, false, true},
		{"release", true, true, true},
		{"later", true, true, true}, // on GitHub since the listing
		{"hotfix", false, true, true},
		{"prod", true, true, true}, // deploy, and not in GitHub's listing
		{"fix", false, false, false},
	} {
		if p.IsProtected(c.branch, c.onGitHub) != c.protected || p.PROnly(c.branch, c.onGitHub) != c.prOnly {
			t.Errorf("%s: protected=%v prOnly=%v", c.branch, p.IsProtected(c.branch, c.onGitHub), p.PROnly(c.branch, c.onGitHub))
		}
	}
	must(s.SetPROnly(ctx, "t", r, false))
	if p, _ = s.Policy(ctx, repo); p.PROnly("main", true) {
		t.Fatal("PR-only still on for main")
	}
}
