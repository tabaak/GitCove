package replay

// Milestone 4 against fakehub: PRs opened, reviewed and merged on GitCove through the web
// interface during a failover, and their replay.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/store"
)

// post submits a web form as the holder of tok and returns the status and the body or redirect.
func (e *env) post(t *testing.T, tok, path string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.cove.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("x", tok)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusSeeOther {
		return resp.StatusCode, resp.Header.Get("Location")
	}
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func (e *env) page(t *testing.T, tok, path string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", e.cove.URL+path, nil)
	req.SetBasicAuth("x", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

// people adds alice, who authorised GitCove to act as alice-gh, and chris, who didn't.
func (e *env) people(t *testing.T) (alice, chris int64, aliceTok, chrisTok string) {
	t.Helper()
	ctx := context.Background()
	var err error
	if alice, aliceTok, err = e.st.CreateToken(ctx, "test", "alice", []string{e.hub.Org + "/app"}); err != nil {
		t.Fatal(err)
	}
	if chris, chrisTok, err = e.st.CreateToken(ctx, "test", "chris", []string{e.hub.Org + "/app"}); err != nil {
		t.Fatal(err)
	}
	users, err := auth.New(e.gh, e.st, slog.New(slog.DiscardHandler), []byte(strings.Repeat("ef", 32)))
	if err != nil {
		t.Fatal(err)
	}
	e.r.Users = users
	if _, err := users.Authorise(ctx, alice, "test", func(code, _ string) { e.hub.ApproveDevice(code, "alice-gh") }); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPullRequestFlow(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	e.hub.Protect(t, "app", "main")
	e.hub.AddIssue("app", "carol", "login breaks", "steps") // #1
	e.hub.AddPR("app", "carol", "feature", "main", "a feature", "")
	_, _, aliceTok, chrisTok := e.people(t)
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	repo := "/" + e.hub.Org + "/app"
	ghMain := e.hub.Refs(t, "app")["refs/heads/main"]

	e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
	e.mode(t, store.Failover)
	dev := e.clone(t, "app")
	fix := work(t, dev, "fix", "fix.txt")
	mustGit(t, dev, "push", "-q", "origin", "fix")
	work(t, dev, "clash", "fix.txt") // the same new file with other content: conflicts with fix
	mustGit(t, dev, "push", "-q", "origin", "clash")

	ok := func(status int, got string) string {
		t.Helper()
		if status != http.StatusSeeOther {
			t.Fatalf("status %d: %s", status, got)
		}
		return got
	}
	refused := func(want string) func(int, string) {
		return func(status int, got string) {
			t.Helper()
			if status != http.StatusBadRequest || !strings.Contains(got, want) {
				t.Fatalf("want a refusal containing %q, got %d: %s", want, status, got)
			}
		}
	}
	if loc := ok(e.post(t, e.tok, repo+"/pulls", url.Values{"head": {"fix"}, "title": {"Fix login"}, "body": {"Fixes #1"}})); loc != repo+"/issues/L-1" {
		t.Fatalf("new PR at %s", loc)
	}
	ok(e.post(t, e.tok, repo+"/pulls", url.Values{"head": {"clash"}, "title": {"Clash"}}))

	// Issues and pull requests, mirrored or queued, each list under their own tab.
	for path, want := range map[string][2]string{"/issues": {"login breaks", "a feature"}, "/pulls": {"a feature", "login breaks"}} {
		if pg := e.page(t, e.tok, repo+path); !strings.Contains(pg, want[0]) || strings.Contains(pg, want[1]) || strings.Contains(pg, "Fix login") != (path == "/pulls") {
			t.Fatalf("GET %s: %s", path, pg)
		}
	}
	if pg := e.page(t, e.tok, repo+"/issues/L-1"); !strings.Contains(pg, `href="`+repo+`/pulls" aria-current="page"`) {
		t.Fatalf("a PR's page doesn't mark the Pull requests tab: %s", pg)
	}
	refused("needs 1 approval")(e.post(t, aliceTok, repo+"/issues/L-1/merge", nil))

	if files := e.page(t, aliceTok, repo+"/issues/L-1/files"); !strings.Contains(files, `name="c|RIGHT|1|fix.txt"`) || !strings.Contains(files, fix) {
		t.Fatalf("files page: %s", files)
	}
	refused("your own PR")(e.post(t, e.tok, repo+"/issues/L-1/review", url.Values{"verdict": {"approve"}, "commit": {fix}}))
	refused("new commits")(e.post(t, aliceTok, repo+"/issues/L-1/review", url.Values{"verdict": {"approve"}, "commit": {ghMain}}))
	ok(e.post(t, aliceTok, repo+"/issues/L-1/review", url.Values{"verdict": {"approve"}, "commit": {fix}, "body": {"looks good"},
		"c|RIGHT|1|fix.txt": {"why this value?"}}))
	ok(e.post(t, chrisTok, repo+"/issues/L-1/review", url.Values{"verdict": {"approve"}, "commit": {fix}}))
	var aliceReview store.Item
	for _, it := range e.metaItems(t) {
		if it.Kind == "review" && it.Change.Author == "alice" {
			aliceReview = it
		}
	}
	ok(e.post(t, chrisTok, repo+"/issues/L-1/reply", url.Values{"parent": {aliceReview.OpID + "/0"}, "body": {"it's the default"}}))
	refused("no such line comment")(e.post(t, chrisTok, repo+"/issues/L-1/reply", url.Values{"parent": {"c999"}, "body": {"x"}}))

	// A mirrored PR: alice approves it, and then it moves on GitHub, so her approval is stale.
	feature := e.served(t, "app", "refs/heads/feature")
	ok(e.post(t, aliceTok, repo+"/issues/2/review", url.Values{"verdict": {"approve"}, "commit": {feature}}))

	ok(e.post(t, aliceTok, repo+"/issues/L-1/merge", nil))
	merged := e.served(t, "app", "refs/heads/main")
	if parents := mustGit(t, e.r.Git.Dir(fakehub.RepoID("app")), "rev-list", "--parents", "-n1", merged); parents != merged+" "+ghMain+" "+fix {
		t.Fatalf("main on GitCove after the merge: %s", parents)
	}
	refused("already merged")(e.post(t, aliceTok, repo+"/issues/L-1/merge", nil))
	ok(e.post(t, aliceTok, repo+"/issues/L-2/review", url.Values{"verdict": {"approve"}, "commit": {e.served(t, "app", "refs/heads/clash")}}))
	refused("conflicts with main")(e.post(t, aliceTok, repo+"/issues/L-2/merge", nil))
	if e.served(t, "app", "refs/heads/main") != merged {
		t.Fatal("a conflicting merge moved main")
	}
	if pg := e.page(t, e.tok, repo+"/issues/L-1"); !strings.Contains(pg, "Merged into main on GitCove by alice") ||
		!strings.Contains(pg, "approved on GitCove, not checked by GitHub") || !strings.Contains(pg, "it&#39;s the default") {
		t.Errorf("PR page: %s", pg)
	}
	if pg := e.page(t, e.tok, repo+"/issues/1"); !strings.Contains(pg, "closed by merging L-1 on GitCove") {
		t.Errorf("linked issue page: %s", pg)
	}
	pg := e.page(t, e.tok, repo+"/changes")
	for _, want := range []string{`href="` + repo + `/changes" aria-current="page"`, "created", `<code class="branch">fix</code>`,
		"opened pull request", "Fix login", "approved pull request", "merged pull request", "into main on GitCove", "closed issue", `aria-label="Approval"`, "queued"} {
		if !strings.Contains(pg, want) {
			t.Errorf("changes page lacks %q: %s", want, pg)
		}
	}
	mustGit(t, dev, "fetch", "-q", "origin")
	mustGit(t, dev, "checkout", "-q", "-B", "direct", "origin/main") // contains the merge, so the push would fast-forward
	mustGit(t, dev, "commit", "-q", "--allow-empty", "-m", "straight to main")
	if out, err := push(dev, "direct:main"); err == nil || !strings.Contains(out, "protected") {
		t.Fatalf("push to main: %v %s", err, out)
	}

	e.hub.SetFaults(fakehub.Faults{})
	ghFeature := e.hub.Commit(t, "app", "feature")
	e.replayAll(t)

	for _, it := range e.metaItems(t) {
		if it.State != "done" {
			t.Errorf("item %d %s on %s ended %s: %s", it.ID, it.Kind, it.Issue, it.State, it.Result)
		}
	}
	gh := e.hub.Refs(t, "app")
	if gh["refs/heads/main"] != ghMain || gh["refs/heads/fix"] != fix {
		t.Errorf("GitHub main=%s fix=%s", gh["refs/heads/main"], gh["refs/heads/fix"])
	}
	prs := e.hub.PRs("app")
	if len(prs) != 3 || prs[1].Head != "fix" || prs[1].Base != "main" || prs[1].State != "open" || !strings.Contains(prs[1].Body, "Fixes #1") {
		t.Fatalf("PRs on GitHub: %+v", prs)
	}
	n := prs[1].Number
	if e.hub.Issues("app")[0].State != "open" {
		t.Error("replay closed the linked issue itself; GitHub does that when the PR merges")
	}
	var applied, told, stale int
	for _, v := range e.hub.Reviews("app") {
		switch {
		case v.Number == n && v.Author == "alice-gh" && v.State == "APPROVED" && v.Commit == fix && strings.Contains(v.Body, "looks good"):
			applied++
		case v.Number == n && v.Author == fakehub.Bot && v.State == "COMMENTED" && strings.Contains(v.Body, "Approved on GitCove") && strings.Contains(v.Body, "`chris`"):
			told++
		case v.Number == 2 && v.Author == "alice-gh" && v.State == "COMMENTED" && v.Commit == feature && strings.Contains(v.Body, "Not applied"):
			stale++
		case v.Number == prs[2].Number && v.State == "APPROVED":
		default:
			t.Errorf("unexpected review %+v", v)
		}
	}
	if applied != 1 || told != 1 || stale != 1 {
		t.Errorf("reviews: %d applied, %d told, %d stale; %+v (feature now %s)", applied, told, stale, e.hub.Reviews("app"), ghFeature)
	}
	lines := e.hub.ReviewComments("app")
	if len(lines) != 2 || lines[0].Path != "fix.txt" || lines[0].Line != 1 || lines[0].Body != "why this value?" ||
		lines[1].InReplyTo != lines[0].ID || lines[1].Author != fakehub.Bot || !strings.Contains(lines[1].Body, "it's the default") {
		t.Errorf("line comments: %+v", lines)
	}
	if c := commentsOn(e.hub, n); len(c) != 1 || c[0].Author != "alice-gh" || !strings.Contains(c[0].Body, "Merged into `main` on GitCove") {
		t.Errorf("merge note: %+v", c)
	}
	// Back to normal: main follows GitHub again, and the work waits there as a PR.
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if e.served(t, "app", "refs/heads/main") != ghMain {
		t.Error("main on GitCove didn't return to GitHub's after replay")
	}
	if rs, err := e.st.Reviews(ctx, e.repoID(t), n); err != nil || len(rs) != 2 {
		t.Errorf("mirrored reviews of #%d: %+v %v", n, rs, err)
	}
}

// A queued review's op ID is hex, so a reply's parent "<op ID>/<index>" can start with "c" like a
// GitHub comment's "c<ID>"; one in 16 replies to queued reviews did, and failed every pass.
func TestReplyParentFormat(t *testing.T) {
	for parent, want := range map[string]bool{"c123": true, "cf6575cd91d2086fbe472b78e4398381/0": false, "0f65/2": false} {
		if got := githubParent(parent); got != want {
			t.Errorf("githubParent(%q) = %v", parent, got)
		}
	}
}
