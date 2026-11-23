package replay

// The Code tab: files, a README, a file, commits and a commit, from the branches GitCove serves.

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodePages(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	dev := filepath.Join(t.TempDir(), "dev")
	mustGit(t, "", "clone", "-q", e.hub.Dir("app"), dev)
	mustGit(t, dev, "checkout", "-q", "-b", "topic/x")
	for name, body := range map[string]string{"README.md": "# App\nhello <b>x</b> [guide](docs/guide.txt) [bad](javascript:alert(1))", "docs/guide.txt": "one\ntwo\n",
		"a#b.txt": "hash", "bin.dat": "a\x00b", "main.go": "package main\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dev, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dev, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, dev, "add", "-A")
	mustGit(t, dev, "commit", "-q", "-m", "Add docs")
	sha := strings.TrimSpace(mustGit(t, dev, "rev-parse", "HEAD"))
	mustGit(t, dev, "push", "-q", "origin", "topic/x")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	repo := "/" + e.hub.Org + "/app"

	has := func(path string, want ...string) {
		t.Helper()
		body := e.page(t, e.tok, path)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Fatalf("GET %s: no %q in %s", path, w, body)
			}
		}
	}
	status := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", e.cove.URL+path, nil)
		req.SetBasicAuth("x", e.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	has(repo, "Code", "data-", e.hub.Org+"/app.git", "Commits") // main's top directory, its clone URL and history link
	has(repo+"/tree/topic/x", `/tree/topic/x/docs"`, `/blob/topic/x/a%23b.txt"`, "Add docs",
		"<h1>App</h1>", `<a href="`+repo+`/blob/topic/x/docs/guide.txt">guide</a>`) // the README rendered, its links into the branch
	if body := e.page(t, e.tok, repo+"/tree/topic/x"); strings.Contains(body, "<b>x") || strings.Contains(body, "javascript:") {
		t.Fatalf("the README's HTML or script link got through: %s", body)
	}
	has(repo+"/blob/topic/x/README.md", "<h1>App</h1>", `href="?plain=1"`)
	has(repo+"/blob/topic/x/README.md?plain=1", "# App</span>", "&lt;b&gt;")
	has(repo+"/blob/topic/x/main.go", `<span class="hl-k`) // coloured by language
	has(repo+"/tree/topic/x/docs", `aria-label="Parent directory"`, "guide.txt")
	has(repo+"/blob/topic/x/docs/guide.txt", "2 lines", "<td>two</td>")
	has(repo+"/blob/topic/x/bin.dat", "Binary file not shown")
	has(repo+"/commits/topic/x", "Add docs", "Commits on", "/commit/"+sha)
	has(repo+"/commits/topic/x/docs", "History for", "Add docs")
	has(repo+"/commit/"+sha, "Add docs", "Showing 5 changed files", "docs/guide.txt")
	if got := status(repo + "/commit/" + sha[:10]); got != http.StatusOK { // a short ID redirects to the full one
		t.Fatalf("short commit ID: %d", got)
	}

	// A commit only a hidden ref reaches, as a force-pushed secret would be, stays unseen.
	r, err := e.st.RepoByName(ctx, e.hub.Org, "app")
	if err != nil {
		t.Fatal(err)
	}
	bare := e.m.Git.Dir(r.GitHubID)
	hidden := strings.TrimSpace(mustGit(t, bare, "commit-tree", sha+"^{tree}", "-m", "secret"))
	mustGit(t, bare, "update-ref", "refs/gitcove/history/20260101T000000.000Z/heads/gone", hidden)
	for path, want := range map[string]int{
		repo + "/commit/" + hidden:         http.StatusNotFound,
		repo + "/tree/nope":                http.StatusNotFound,
		repo + "/blob/topic/x/missing.txt": http.StatusNotFound,
		repo + "/commit/HEAD":              http.StatusNotFound,
	} {
		if got := status(path); got != want {
			t.Errorf("GET %s: %d, want %d", path, got, want)
		}
	}
	mustGit(t, "", "clone", "-q", strings.Replace(e.cove.URL, "http://", "http://x:"+e.tok+"@", 1)+repo+".git", filepath.Join(t.TempDir(), "c"))
}
