package mirror

// Milestone 6 exit, code half: a force-pushed branch can be brought back and survives the next
// sync, and clients can't fetch hidden history by commit ID.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/gitstore"
)

// forcePush points branch at a new commit that doesn't contain its old tip.
func forcePush(t *testing.T, hub *fakehub.Hub, repo, branch string) string {
	sha := hub.Commit(t, repo, "scratch")
	mustGit(t, hub.Dir(repo), "update-ref", "refs/heads/"+branch, sha)
	mustGit(t, hub.Dir(repo), "update-ref", "-d", "refs/heads/scratch")
	return sha
}

func TestRestoreForcePushedBranch(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 1, "app")
	lost := e.hub.Commit(t, "app", "topic")
	gone := e.hub.Commit(t, "app", "doomed")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	e.hub.Commit(t, "app", "main") // a fast-forward saves nothing
	forcePush(t, e.hub, "app", "topic")
	mustGit(t, e.hub.Dir("app"), "update-ref", "-d", "refs/heads/doomed")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	id := fakehub.RepoID("app")
	saved, err := e.m.Git.History(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].Branch != "doomed" || saved[0].Commit != gone || saved[1].Branch != "topic" || saved[1].Commit != lost {
		t.Fatalf("history = %+v, want doomed at %s and topic at %s", saved, gone, lost)
	}

	_, tok, err := e.st.CreateToken(ctx, "test", "dev", []string{"fakeorg/app"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0", "1", "2"} {
		w := t.TempDir()
		mustGit(t, w, "init", "-q")
		if out, err := git(t, w, "-c", "protocol.version="+v, "fetch", e.url(tok, "app"), lost); err == nil {
			t.Fatalf("protocol v%s fetched hidden history by commit ID: %s", v, out)
		}
	}
	// The client refuses before asking, so also ask the server directly, the way a hostile client would.
	want := fmt.Sprintf("%04xwant %s\n", 4+len("want \n")+len(lost), lost)
	for name, c := range map[string]struct{ proto, body string }{
		"v0": {"", want + "0000" + "0009done\n"},
		"v2": {"version=2", "0014command=fetch\n0001" + want + "0009done\n0000"},
	} {
		req, _ := http.NewRequest("POST", e.cove.URL+"/fakeorg/app.git/git-upload-pack", strings.NewReader(c.body))
		req.SetBasicAuth("x", tok)
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		req.Header.Set("Git-Protocol", c.proto)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), "PACK") {
			t.Fatalf("%s: server sent a pack for a hidden commit", name)
		}
	}

	name, sha, err := e.m.Git.Restore(ctx, id, "topic", saved[1].At)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, gitstore.RestoredPrefix+"topic-") || sha != lost {
		t.Fatalf("restored %s at %s, want gitcove/restored-topic-<stamp> at %s", name, sha, lost)
	}
	if _, _, err := e.m.Git.Restore(ctx, id, "topic", saved[1].At); err == nil {
		t.Fatal("a second restore moved or replaced the restored branch")
	}
	if _, _, err := e.m.Git.Restore(ctx, id, "main", saved[1].At); err == nil {
		t.Fatal("restored a tip that was never saved")
	}
	// A sync straight after a restore, and another once history has expired.
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	e.m.Git.Keep = 0
	e.hub.Commit(t, "app", "main")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.served(t, "app")["refs/heads/"+name]; got != lost {
		t.Fatalf("restored branch after syncs = %q, want %s", got, lost)
	}
	if saved, _ := e.m.Git.History(ctx, id); len(saved) != 0 {
		t.Fatalf("expired history kept: %+v", saved)
	}
	w := t.TempDir()
	mustGit(t, w, "init", "-q")
	if out, err := git(t, w, "fetch", e.url(tok, "app"), name); err != nil {
		t.Fatalf("fetch restored branch: %v: %s", err, out)
	}
}
