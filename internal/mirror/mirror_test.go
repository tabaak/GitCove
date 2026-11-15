package mirror

// Milestone 1 exit criteria, end to end against fakehub: a repo can be cloned from GitCove with
// GitHub blocked, and a fetch cut off mid-way is recovered on the next cycle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/health"
	"github.com/tabaak/gitcove/internal/store"
	"github.com/tabaak/gitcove/internal/web"
)

type env struct {
	hub    *fakehub.Hub
	gh     *ghclient.App
	m      *Mirror
	st     *store.Store
	health *health.Checker
	cove   *httptest.Server
}

func setup(t *testing.T, seed uint64, repos ...string) *env {
	hub := fakehub.New(t, seed)
	for _, r := range repos {
		hub.AddRepo(t, r)
	}
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "cove.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gs, err := gitstore.New(filepath.Join(dir, "repos"), hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	gh := &ghclient.App{API: hub.URL, AppID: hub.AppID, Key: hub.Key, Org: hub.Org, HTTP: &http.Client{Timeout: 5 * time.Second}}
	hc := &health.Checker{GH: gh, HTTP: &http.Client{Timeout: 2 * time.Second}, GitURL: hub.URL, Store: st, Log: log}
	cove := httptest.NewServer((&web.Server{Store: st, Git: gs, Health: hc, Log: log}).Handler())
	t.Cleanup(cove.Close)
	return &env{hub, gh, &Mirror{GH: gh, Store: st, Git: gs, FetchTimeout: 30 * time.Second, Log: log}, st, hc, cove}
}

// served returns the refs GitCove serves for name, in the namespaces fakehub has.
func (e *env) served(t *testing.T, name string) map[string]string {
	refs, err := e.m.Git.Refs(context.Background(), e.m.Git.Dir(fakehub.RepoID(name)))
	if err != nil {
		t.Fatal(err)
	}
	maps.DeleteFunc(refs, func(ref, _ string) bool { return strings.HasPrefix(ref, "refs/gitcove/") })
	return refs
}

func (e *env) url(tok, name string) string {
	return strings.Replace(e.cove.URL, "http://", "http://x:"+tok+"@", 1) + "/" + e.hub.Org + "/" + name + ".git"
}

func git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// A developer running GitCove on their own machine has `cove client on` in ~/.gitconfig, which
// would send GitCove's own fetches from GitHub back to itself.
func TestIgnoresUserGitConfig(t *testing.T) {
	e := setup(t, 1, "app")
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[url \"http://127.0.0.1:9/\"]\n\tinsteadOf = "+e.hub.URL+"/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	if err := e.m.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCloneWhileGitHubDown(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app", "other")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	_, tok, err := e.st.CreateToken(ctx, "test", "dev", []string{"fakeorg/app"})
	if err != nil {
		t.Fatal(err)
	}
	e.health.Check(ctx)
	if st := e.health.Status(); !st["api"].OK || !st["git-https"].OK {
		t.Fatalf("health with GitHub up: %+v", st)
	}

	want := e.hub.Refs(t, "app")
	e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})

	if err := e.m.Cycle(ctx); err == nil {
		t.Fatal("sync succeeded with GitHub down")
	}
	if got := e.served(t, "app"); !maps.Equal(got, want) {
		t.Fatalf("failed sync changed served refs:\n got %v\nwant %v", got, want)
	}

	work := t.TempDir()
	if out, err := git(t, work, "clone", "-q", e.url(tok, "app"), "app"); err != nil {
		t.Fatalf("clone with GitHub down: %v: %s", err, out)
	}
	clone := filepath.Join(work, "app")
	if head, _ := git(t, clone, "rev-parse", "HEAD"); head != want["refs/heads/main"] {
		t.Fatalf("clone HEAD = %s, want main %s", head, want["refs/heads/main"])
	}
	remote, _ := git(t, clone, "ls-remote", "origin")
	if !strings.Contains(remote, "refs/pull/1/head") || !strings.Contains(remote, "refs/tags/v1") || strings.Contains(remote, "refs/gitcove") {
		t.Fatalf("ls-remote shows wrong refs:\n%s", remote)
	}
	if out, err := git(t, clone, "push", "origin", "HEAD:refs/heads/new"); err == nil {
		t.Fatalf("push outside failover was accepted: %s", out)
	}

	e.health.Check(ctx)
	resp, err := http.Get(e.cove.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Repos  int
		GitHub struct{ API, Git health.Result }
	}
	err = json.NewDecoder(resp.Body).Decode(&status)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if status.Repos != 2 || status.GitHub.API.OK || status.GitHub.Git.OK {
		t.Fatalf("status with GitHub down = %+v", status)
	}

	// Access control.
	_, otherTok, err1 := e.st.CreateToken(ctx, "test", "other", []string{"fakeorg/other"})
	revokedID, revoked, err2 := e.st.CreateToken(ctx, "test", "revoked", []string{"fakeorg/app"})
	if err := errors.Join(err1, err2, e.st.RevokeToken(ctx, "test", revokedID)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, tok, repo, service string
		want                     int
	}{
		{"no token", "", "app", "git-upload-pack", 401},
		{"wrong token", "cove_nope", "app", "git-upload-pack", 401},
		{"revoked", revoked, "app", "git-upload-pack", 401},
		{"no access", otherTok, "app", "git-upload-pack", 404},
		{"unknown repo", tok, "nope", "git-upload-pack", 404},
		{"push", tok, "app", "git-receive-pack", 403},
		{"ok", tok, "app", "git-upload-pack", 200},
	} {
		req, _ := http.NewRequest("GET", e.cove.URL+"/fakeorg/"+c.repo+".git/info/refs?service="+c.service, nil)
		if c.tok != "" {
			req.SetBasicAuth("x", c.tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

func TestRecoversFromCutFetch(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 2, "app")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	before := e.served(t, "app")
	next := e.hub.Commit(t, "app", "main")

	e.hub.SetFaults(fakehub.Faults{CutRate: 1})
	if err := e.m.Cycle(ctx); err == nil {
		t.Fatal("sync succeeded through a cut connection")
	}
	if got := e.served(t, "app"); !maps.Equal(got, before) {
		t.Fatalf("cut fetch moved served refs:\n got %v\nwant %v", got, before)
	}
	e.hub.SetFaults(fakehub.Faults{})
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatalf("next cycle: %v", err)
	}
	if got := e.served(t, "app")["refs/heads/main"]; got != next {
		t.Fatalf("main = %s after recovery, want %s", got, next)
	}
}

// TestFaultCampaign runs seeded rounds of GitHub faults and GitHub-side changes. After every round
// the store must be intact, and once faults stop one cycle must bring it level with GitHub.
// COVE_SEEDS sets the number of seeds (default 10; the nightly run uses 200).
// Repeat one with: COVE_SEEDS=N go test -run 'TestFaultCampaign/seed=K$' ./internal/mirror
func TestFaultCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			campaign(t, seed)
		})
	}
}

func campaign(t *testing.T, seed uint64) {
	ctx := context.Background()
	repos := []string{"app", "lib"}
	e := setup(t, seed, repos...)
	rng := mrand.New(mrand.NewPCG(seed, 99))
	// Short timeouts while faulty, so hung requests cost little.
	e.gh.HTTP.Timeout, e.m.FetchTimeout = 500*time.Millisecond, 3*time.Second

	failed := 0
	for round := range 6 {
		f := fakehub.Faults{
			APIDown:     rng.Float64() < 0.1,
			GitDown:     rng.Float64() < 0.1,
			ErrorRate:   rng.Float64() * 0.2,
			TimeoutRate: rng.Float64() * 0.1,
			CutRate:     rng.Float64() * 0.3,
		}
		e.hub.SetFaults(f)
		mutate(t, e.hub, rng, repos[rng.IntN(len(repos))])
		cycleErr := e.m.Cycle(ctx)
		if cycleErr != nil {
			failed++
		}
		for _, r := range repos {
			if _, statErr := os.Stat(e.m.Git.Dir(fakehub.RepoID(r))); statErr != nil {
				continue // never fetched yet
			}
			if out, err := git(t, e.m.Git.Dir(fakehub.RepoID(r)), "fsck", "--connectivity-only", "--no-dangling"); err != nil {
				t.Fatalf("round %d (faults %+v, cycle err %v): %s store broken: %s", round, f, cycleErr, r, out)
			}
		}
	}

	t.Logf("%d of 6 faulty rounds failed to sync", failed)
	e.hub.SetFaults(fakehub.Faults{})
	e.gh.HTTP.Timeout, e.m.FetchTimeout = 30*time.Second, 30*time.Second
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatalf("clean cycle after faults: %v", err)
	}
	for _, r := range repos {
		if got, want := e.served(t, r), e.hub.Refs(t, r); !maps.Equal(got, want) {
			t.Fatalf("%s served refs differ from GitHub:\n got %v\nwant %v", r, got, want)
		}
	}
}

// mutate makes one GitHub-side change: new commits, a new branch, a deleted branch or a force-push.
func mutate(t *testing.T, hub *fakehub.Hub, rng *mrand.Rand, repo string) {
	refs := hub.Refs(t, repo)
	switch rng.IntN(4) {
	case 0:
		hub.Commit(t, repo, "main")
	case 1:
		hub.Commit(t, repo, fmt.Sprintf("b%d", rng.IntN(1000)))
	case 2:
		if _, ok := refs["refs/heads/feature"]; ok {
			mustGit(t, hub.Dir(repo), "update-ref", "-d", "refs/heads/feature")
		}
	case 3: // force-push feature to an unrelated commit
		sha := hub.Commit(t, repo, "scratch")
		mustGit(t, hub.Dir(repo), "update-ref", "refs/heads/feature", sha)
		mustGit(t, hub.Dir(repo), "update-ref", "-d", "refs/heads/scratch")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := git(t, dir, args...); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
