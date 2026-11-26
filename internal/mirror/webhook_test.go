package mirror

// Milestone 6 exit, sync half: with dropped, repeated and out-of-order webhooks a second full sync
// finds no unexplained differences. Also the full scan that finds deletions.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/store"
)

const webhookSecret = "whsecret"

type delivery struct {
	id, repo string
	deleted  bool
}

// post sends d to GitCove as GitHub would, signed with secret, and returns the status.
func (e *env) post(t *testing.T, d delivery, secret string) int {
	body := fmt.Sprintf(`{"action":%q,"repository":{"id":%d}}`, map[bool]string{true: "deleted", false: "edited"}[d.deleted], fakehub.RepoID(d.repo))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req, _ := http.NewRequest("POST", e.cove.URL+"/webhook", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", d.id)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// state is what GitCove or GitHub has for a repo: refs, issues and comments, each as one string.
func (e *env) state(t *testing.T, name string) map[string]string {
	ctx := context.Background()
	out := map[string]string{}
	for ref, sha := range e.served(t, name) {
		out[ref] = sha
	}
	r, err := e.st.RepoByName(ctx, e.hub.Org, name)
	if err != nil {
		t.Fatal(err)
	}
	issues, err := e.st.Issues(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range issues {
		out[fmt.Sprintf("issue %d", i.Number)] = i.Title + "|" + i.Body + "|" + i.State
		cs, err := e.st.Comments(ctx, r.ID, i.Number)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			out[fmt.Sprintf("comment %d", c.ID)] = c.Body
		}
	}
	return out
}

func (e *env) hubState(t *testing.T, name string) map[string]string {
	out := e.hub.Refs(t, name)
	for _, i := range e.hub.Issues(name) {
		if !i.Deleted {
			out[fmt.Sprintf("issue %d", i.Number)] = i.Title + "|" + i.Body + "|" + i.State
		}
	}
	for _, c := range e.hub.Comments(name) {
		out[fmt.Sprintf("comment %d", c.ID)] = c.Body
	}
	return out
}

// TestWebhookCampaign makes seeded GitHub-side changes, each announced by a webhook that may be
// dropped, repeated or delivered out of order, and syncs only what webhooks ask for. A repo whose
// every webhook got through must match GitHub, except items deleted while every "deleted" webhook
// was dropped; then one full sync must bring everything level, and a second must change nothing.
// COVE_SEEDS sets the number of seeds (default 10; the nightly run uses 40).
func TestWebhookCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			webhookCampaign(t, seed)
		})
	}
}

func webhookCampaign(t *testing.T, seed uint64) {
	ctx := context.Background()
	repos := []string{"app", "lib"}
	e := setup(t, seed, repos...)
	rng := mrand.New(mrand.NewPCG(seed, 7))
	for _, r := range repos {
		for i := range 3 {
			n := e.hub.AddIssue(r, "alice", fmt.Sprintf("issue %d", i), "body")
			e.hub.AddComment(r, n, "bob", "first")
		}
	}
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	stale := map[string]bool{}        // repos changed since their last webhook got through
	unburied := map[string][]string{} // per repo: keys of items deleted while every "deleted" webhook was dropped
	var sent []delivery
	next := 0
	for round := range 8 {
		var batch []delivery
		changed := map[string]bool{}
		for range 1 + rng.IntN(3) {
			r := repos[rng.IntN(len(repos))]
			changed[r] = true
			next++
			d := delivery{id: fmt.Sprintf("d-%d", next), repo: r}
			issues, comments := e.hub.Issues(r), e.hub.Comments(r)
			var live []int
			for _, i := range issues {
				if !i.Deleted {
					live = append(live, i.Number)
				}
			}
			if len(live) == 0 {
				live = append(live, e.hub.AddIssue(r, "alice", "refill", "body"))
			}
			n := live[rng.IntN(len(live))]
			switch k := rng.IntN(6); {
			case k == 0:
				mutate(t, e.hub, rng, r)
			case k == 1:
				e.hub.AddComment(r, e.hub.AddIssue(r, "alice", "new", "body"), "bob", "hi")
			case k == 2:
				e.hub.SetIssue(r, n, "title", fmt.Sprintf("edited %d", next))
			case k == 3 && len(comments) > 0:
				c := comments[rng.IntN(len(comments))]
				e.hub.DeleteComment(r, c.ID)
				unburied[r] = append(unburied[r], fmt.Sprintf("comment %d", c.ID))
				d.deleted = true
			case k == 4:
				e.hub.DeleteIssue(r, n)
				unburied[r] = append(unburied[r], fmt.Sprintf("issue %d", n))
				for _, c := range comments {
					if c.Number == n {
						unburied[r] = append(unburied[r], fmt.Sprintf("comment %d", c.ID))
					}
				}
				d.deleted = true
			default:
				e.hub.AddComment(r, n, "carol", fmt.Sprintf("c%d", next))
			}
			batch = append(batch, d)
		}
		// Faults: drop, repeat (same delivery ID, as GitHub's redelivery does), reorder, and replay old ones.
		var wire []delivery
		for _, d := range batch {
			if rng.Float64() < 0.3 {
				continue
			}
			wire = append(wire, d)
			if rng.Float64() < 0.3 {
				wire = append(wire, d)
			}
		}
		if len(sent) > 0 && rng.Float64() < 0.5 {
			wire = append(wire, sent[rng.IntN(len(sent))]) // a late redelivery of an old one
		}
		rng.Shuffle(len(wire), func(i, j int) { wire[i], wire[j] = wire[j], wire[i] })
		for r := range changed {
			stale[r] = true
		}
		for _, d := range wire {
			if got := e.post(t, d, webhookSecret); got != http.StatusAccepted {
				t.Fatalf("delivery %s: status %d", d.id, got)
			}
			if !slicesContains(sent, d) {
				stale[d.repo] = false // any new delivery syncs the whole repo
				if d.deleted {
					unburied[d.repo] = nil // and a "deleted" one makes that sync a full scan
				}
				sent = append(sent, d)
			}
		}
		if got := e.post(t, delivery{id: "forged", repo: "app", deleted: true}, "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("forged delivery: status %d, want 401", got)
		}
		if err := e.m.SyncWanted(ctx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if w, _ := e.st.WantedRepos(ctx); len(w) != 0 {
			t.Fatalf("round %d: repos still wanted after syncing: %v", round, w)
		}
		for _, r := range repos {
			if stale[r] {
				continue
			}
			got, want := e.state(t, r), e.hubState(t, r)
			for _, k := range unburied[r] {
				delete(got, k) // explained: deleted on GitHub, but the webhook saying so was dropped
			}
			if !maps.Equal(got, want) {
				t.Fatalf("round %d: %s differs from GitHub after its webhooks got through:\n got %v\nwant %v", round, r, got, want)
			}
		}
	}

	// A late redelivery of something already synced queues nothing.
	if len(sent) > 0 {
		e.post(t, sent[0], webhookSecret)
		if w, _ := e.st.WantedRepos(ctx); len(w) != 0 {
			t.Fatalf("a repeated delivery queued a sync: %v", w)
		}
	}
	fullSync := func() map[string]map[string]string {
		t.Helper()
		for _, name := range repos {
			r, _ := e.st.RepoByName(ctx, e.hub.Org, name)
			if err := e.st.SetCursor(ctx, store.FullScanCursor(r.ID), store.ScanDue); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.m.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]string{}
		for _, r := range repos {
			out[r] = e.state(t, r)
			if want := e.hubState(t, r); !maps.Equal(out[r], want) {
				t.Fatalf("%s differs from GitHub after a full sync:\n got %v\nwant %v", r, out[r], want)
			}
		}
		return out
	}
	first, second := fullSync(), fullSync()
	for _, r := range repos {
		if !maps.Equal(first[r], second[r]) {
			t.Fatalf("%s: second full sync changed something", r)
		}
	}
}

func slicesContains(ds []delivery, d delivery) bool {
	for _, x := range ds {
		if x.id == d.id {
			return true
		}
	}
	return false
}

// TestFullScanBuries checks the full scan moves deleted issues, comments and line comments to
// tombstones, and leaves alone what GitHub still has.
func TestFullScanBuries(t *testing.T) {
	ctx := context.Background()
	e := setup(t, 3, "app")
	keep := e.hub.AddIssue("app", "alice", "keep", "body")
	doomed := e.hub.AddIssue("app", "alice", "doomed", "body")
	e.hub.AddComment("app", keep, "bob", "stays")
	e.hub.AddComment("app", keep, "bob", "goes")
	e.hub.AddComment("app", doomed, "bob", "goes with its issue")
	pr := e.hub.AddPR("app", "alice", "feature", "main", "pr", "")
	line := e.hub.AddReviewComment("app", pr, "bob", "f.go", 1, "nit")
	e.hub.AddReviewComment("app", pr, "bob", "f.go", 2, "stays too")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	e.hub.DeleteComment("app", e.hub.Comments("app")[1].ID)
	e.hub.DeleteIssue("app", doomed)
	e.hub.DeleteReviewComment("app", line)
	r, _ := e.st.RepoByName(ctx, e.hub.Org, "app")
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, "app"); maps.Equal(got, e.hubState(t, "app")) {
		t.Fatal("a sync before the scan is due saw deletions; the test proves nothing")
	}
	if err := e.st.SetCursor(ctx, store.FullScanCursor(r.ID), store.ScanDue); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := e.state(t, "app"), e.hubState(t, "app"); !maps.Equal(got, want) {
		t.Fatalf("after the full scan:\n got %v\nwant %v", got, want)
	}
	if rc, _ := e.st.ReviewComments(ctx, r.ID, pr); len(rc) != 1 || rc[0].Body != "stays too" {
		t.Fatalf("line comments after the scan: %+v", rc)
	}
	db, err := sql.Open("sqlite", e.st.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM tombstones WHERE repo_id = ?", r.ID).Scan(&n); err != nil || n != 4 {
		t.Fatalf("tombstones = %d (%v), want 4: an issue, two comments and a line comment", n, err)
	}
}
