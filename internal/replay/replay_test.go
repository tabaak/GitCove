package replay

// Milestone 2 against fakehub: pushes during failover, and replay of them to GitHub.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/health"
	"github.com/tabaak/gitcove/internal/mirror"
	"github.com/tabaak/gitcove/internal/store"
	"github.com/tabaak/gitcove/internal/web"
)

// The pre-receive hook runs "<this test binary> hook pre-receive".
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "hook" {
		os.Exit(gitstore.RunHook(os.Args[2:]))
	}
	os.Exit(m.Run())
}

type env struct {
	hub  *fakehub.Hub
	gh   *ghclient.App
	st   *store.Store
	m    *mirror.Mirror
	r    *Replayer
	cove *httptest.Server
	tok  string
}

func setup(t *testing.T, seed uint64, repo string) *env {
	ctx := context.Background()
	hub := fakehub.New(t, seed)
	hub.AddRepo(t, repo)
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
	gh := &ghclient.App{API: hub.URL, AppID: hub.AppID, Key: hub.Key, Org: hub.Org, HTTP: &http.Client{Timeout: 5 * time.Second},
		Web: hub.URL, ClientID: fakehub.ClientID, ClientSecret: fakehub.ClientSecret, Budget: ghclient.Budget{PerMinute: 1e6, PerHour: 1e6}}
	m := &mirror.Mirror{GH: gh, Store: st, Git: gs, FetchTimeout: 30 * time.Second, Log: log}
	hc := &health.Checker{GH: gh, HTTP: &http.Client{Timeout: time.Second}, GitURL: hub.URL, Store: st, Log: log}
	cove := httptest.NewServer((&web.Server{Store: st, Git: gs, Health: hc, Log: log}).Handler())
	t.Cleanup(cove.Close)
	e := &env{hub: hub, gh: gh, st: st, m: m, cove: cove,
		r: &Replayer{GH: gh, Store: st, Git: gs, Mirror: m, Log: log, MarkerWait: 100 * time.Millisecond}}
	if err := m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if _, e.tok, err = st.CreateToken(ctx, "test", "ben", []string{hub.Org + "/" + repo}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) mode(t *testing.T, to ...store.Mode) {
	t.Helper()
	for _, m := range to {
		if err := e.st.SetMode(context.Background(), "test", m); err != nil {
			t.Fatal(err)
		}
	}
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=dev", "GIT_AUTHOR_EMAIL=dev@example.invalid",
		"GIT_COMMITTER_NAME=dev", "GIT_COMMITTER_EMAIL=dev@example.invalid")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

// clone makes a developer's clone of repo from GitCove.
func (e *env) clone(t *testing.T, repo string) string {
	dir := filepath.Join(t.TempDir(), repo)
	url := strings.Replace(e.cove.URL, "http://", "http://x:"+e.tok+"@", 1) + "/" + e.hub.Org + "/" + repo + ".git"
	mustGit(t, "", "clone", "-q", url, dir)
	return dir
}

// work checks out branch from GitCove (or from main if it's new), commits a file, and returns the commit.
func work(t *testing.T, dir, branch, file string) string {
	mustGit(t, dir, "fetch", "-q", "origin")
	from := "origin/" + branch
	if _, err := git(dir, "rev-parse", "-q", "--verify", from); err != nil {
		from = "origin/main"
	}
	mustGit(t, dir, "checkout", "-q", "-B", branch, from)
	b := make([]byte, 8)
	rand.Read(b)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(hex.EncodeToString(b)), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "work on "+branch)
	return mustGit(t, dir, "rev-parse", "HEAD")
}

func push(dir string, refspec string) (string, error) {
	return git(dir, "push", "-q", "origin", refspec)
}

func (e *env) items(t *testing.T) map[string][]store.RefItem {
	all, err := e.st.RefItems(context.Background(), 0, "pending", "sending", "done", "skipped", "conflict", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string][]store.RefItem{}
	for _, it := range all {
		by[it.Branch] = append(by[it.Branch], it)
	}
	return by
}

func (e *env) served(t *testing.T, repo, ref string) string {
	sha, err := e.r.Git.Ref(context.Background(), fakehub.RepoID(repo), ref)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// onGitHub reports whether some branch on fakehub contains sha.
func onGitHub(hub *fakehub.Hub, repo, sha string) bool {
	out, err := git(hub.Dir(repo), "for-each-ref", "--contains", sha, "refs/heads/")
	return err == nil && out != ""
}

func TestPushesDuringFailoverAndReplay(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.hub.Commit(t, "app", "release")
	e.hub.Protect(t, "app", "release")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	dev := e.clone(t, "app")

	// Outside failover.
	work(t, dev, "fix", "fix.txt")
	if out, err := push(dev, "fix"); err == nil || !strings.Contains(out, "only during failover") {
		t.Fatalf("push in normal mode: %v: %s", err, out)
	}

	e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	e.mode(t, store.Failover)

	tips := map[string]string{}
	accept := func(branch, file string) {
		t.Helper()
		tips[branch] = work(t, dev, branch, file)
		if out, err := push(dev, branch); err != nil {
			t.Fatalf("push %s: %v: %s", branch, err, out)
		}
	}
	refuse := func(refspec, want string) {
		t.Helper()
		if out, err := push(dev, refspec); err == nil || !strings.Contains(out, want) {
			t.Fatalf("push %s: want refusal containing %q, got %v: %s", refspec, want, err, out)
		}
	}
	accept("fix", "fix.txt")
	accept("feature", "feature.txt")
	accept("dup", "dup.txt")   // before main, so it doesn't contain main's new commit
	accept("main", "main.txt") // unprotected default branch: accepted, replayed as a PR
	accept("wf", ".github/workflows/ci.yml")
	work(t, dev, "release", "r.txt")
	refuse("release", "release is protected")
	refuse("+"+tips["fix"]+"~1:refs/heads/fix", "force-push refused")
	refuse(":refs/heads/feature", "can't be deleted")
	mustGit(t, dev, "tag", "t1")
	refuse("refs/tags/t1", "only branches")
	refuse("HEAD:refs/gitcove/github/heads/main", "") // hidden namespace

	// GitHub moves feature on meanwhile, but not main: main's work could fast-forward, and must
	// still arrive as a PR. A developer also pushes "dup" to GitHub directly.
	e.hub.SetFaults(fakehub.Faults{})
	ghMain := e.hub.Refs(t, "app")["refs/heads/main"]
	ghFeature := e.hub.Commit(t, "app", "feature")
	ghOther := e.hub.Commit(t, "app", "other")
	mustGit(t, e.hub.Dir("app"), "fetch", "-q", e.r.Git.Dir(fakehub.RepoID("app")), "refs/heads/dup:refs/heads/dup")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e.served(t, "app", "refs/heads/feature") != tips["feature"] || e.served(t, "app", "refs/heads/other") != ghOther {
		t.Fatal("sync during failover moved a branch with unreplayed work, or didn't move one without")
	}

	e.mode(t, store.Reconciling)
	plan, errs, err := e.r.Plan(ctx)
	if err != nil || len(errs) > 0 {
		t.Fatal(err, errs)
	}
	want := map[string]Kind{"fix": FastForward, "feature": PR, "main": PR, "dup": Already, "wf": Conflict}
	for _, a := range plan {
		if want[a.Branch] != a.Kind {
			t.Errorf("plan for %s: %s (%s), want %s", a.Branch, a.Kind, a.Reason, want[a.Branch])
		}
		delete(want, a.Branch)
	}
	if len(want) > 0 {
		t.Errorf("plan lacks %v", want)
	}

	e.mode(t, store.Replaying)
	if err := e.r.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := e.st.Mode(ctx); m != store.Normal {
		t.Fatalf("mode after replay = %s", m)
	}

	gh := e.hub.Refs(t, "app")
	if gh["refs/heads/fix"] != tips["fix"] || gh["refs/heads/main"] != ghMain {
		t.Fatalf("GitHub fix=%s main=%s", gh["refs/heads/fix"], gh["refs/heads/main"])
	}
	prs := map[string]fakehub.PR{}
	for _, pr := range e.hub.PRs("app") {
		prs[pr.Base] = pr
	}
	if len(prs) != 2 || gh["refs/heads/"+prs["main"].Head] != tips["main"] || gh["refs/heads/"+prs["feature"].Head] != tips["feature"] ||
		!strings.Contains(prs["main"].Body, "`ben`") || !strings.Contains(prs["main"].Body, "gitcove:op=") {
		t.Fatalf("PRs = %+v", e.hub.PRs("app"))
	}
	for _, p := range e.hub.Pushes("app") {
		if p.Ref == "refs/heads/main" || p.Ref == "refs/heads/feature" || p.Ref == "refs/heads/dup" {
			t.Errorf("replay pushed to %s", p.Ref)
		}
	}
	items := e.items(t)
	if items["wf"][0].State != "conflict" || !strings.Contains(items["wf"][0].Result, "workflows") || items["dup"][0].Result != string(Already) {
		t.Fatalf("items: wf %+v, dup %+v", items["wf"], items["dup"])
	}

	// Back to normal: branches whose work reached GitHub follow it again; the conflict stays put.
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e.served(t, "app", "refs/heads/main") != ghMain || e.served(t, "app", "refs/heads/feature") != ghFeature ||
		e.served(t, "app", "refs/heads/wf") != tips["wf"] {
		t.Fatal("served refs after replay")
	}
	// An admin skips the conflict; the branch is then GitHub's (which has none).
	if err := e.st.Resolve(ctx, "test", items["wf"][0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.served(t, "app", "refs/heads/wf"); got != "" {
		t.Fatalf("skipped branch still served at %s", got)
	}
}

// TestAcceptedPushMessage: a developer whose push is accepted is told when it reaches GitHub, and
// that default-branch work gets there as a PR.
func TestAcceptedPushMessage(t *testing.T) {
	e := setup(t, 1, "app")
	dev := e.clone(t, "app")
	for _, c := range []struct {
		mode         store.Mode
		branch, want string
	}{
		{store.Degraded, "fix", "will be sent to GitHub shortly."},
		{store.Paused, "fix", "will be sent to GitHub once the admin resumes replay."},
		{store.Failover, "fix", "will be sent to GitHub when it's back."},
		{store.Failover, "main", "will be sent to GitHub when it's back, as a PR."},
	} {
		if m, _ := e.st.Mode(context.Background()); m != c.mode {
			e.mode(t, c.mode)
		}
		work(t, dev, c.branch, c.branch+"-"+string(c.mode)+".txt")
		if out, err := push(dev, c.branch); err != nil || !strings.Contains(out, "remote: GitCove: ") || !strings.Contains(out, c.want) {
			t.Fatalf("%s push to %s: %v: %s\nwant %q", c.mode, c.branch, err, out, c.want)
		}
	}
}
