package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestTokensAndAudit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cove.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.UpsertRepo(ctx, 100, "org", "a", "main")
	b, _ := s.UpsertRepo(ctx, 200, "org", "b", "main")
	if again, _ := s.UpsertRepo(ctx, 100, "org", "a-renamed", "trunk"); again != a {
		t.Fatalf("upsert by github_id gave new id %d, want %d", again, a)
	}

	if _, _, err := s.CreateToken(ctx, "test", "bad", []string{"org/missing"}); err == nil {
		t.Fatal("token for unknown repo was created")
	}
	id, plain, err := s.CreateToken(ctx, "test", "alice", []string{"org/a-renamed"})
	if err != nil {
		t.Fatal(err)
	}
	can := func(tok string, repo int64) bool {
		ok, err := s.TokenCanRead(ctx, tok, repo)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !can(plain, a) || can(plain, b) || can("cove_wrong", a) {
		t.Fatal("token access wrong before revoke")
	}
	if err := s.RevokeToken(ctx, "test", id); err != nil {
		t.Fatal(err)
	}
	if can(plain, a) {
		t.Fatal("revoked token still reads")
	}
	if err := s.RevokeToken(ctx, "test", id); err == nil {
		t.Fatal("second revoke succeeded")
	}

	// Reopen: migrations must not reapply, and data must survive.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log, err := s.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 || log[0].Action != "token.create" || log[1].Action != "token.revoke" {
		t.Fatalf("audit log = %+v", log)
	}
	toks, _ := s.Tokens(ctx)
	if len(toks) != 1 || toks[0].RevokedAt == "" || len(toks[0].Repos) != 1 {
		t.Fatalf("tokens = %+v", toks)
	}
}
