package replay

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/store"
)

// TestPullCampaign runs seeded failovers in which people push branches, open PRs, review them
// with line comments, reply, approve and merge locally, while on GitHub PR branches and main
// move, PRs are closed or merged, line comments are deleted and a user token is revoked. Replay
// then runs with the faults of TestIssueCampaign. Afterwards it checks all six of plan section
// 3: nothing lost (R2), nothing duplicated (R3), GitHub's refs move only by fast-forward and
// never main (R1, R4), no approval but a person's own for the exact commit (R7), and a full sync
// agrees. COVE_SEEDS sets the number of seeds (default 10; the nightly run uses 40).
// Repeat one with: COVE_SEEDS=N go test -run 'TestPullCampaign/seed=K$' ./internal/replay
func TestPullCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			pullCampaign(t, seed, false)
		})
	}
}

// pullCampaign runs failovers, or with degraded set, rounds of degraded mode (see TestDegradedCampaign).
func pullCampaign(t *testing.T, seed uint64, degraded bool) {
	ctx := context.Background()
	e := setup(t, seed, "app")
	rng := mrand.New(mrand.NewPCG(seed, 13))
	repoID := e.repoID(t)
	repo := "/" + e.hub.Org + "/app"
	hubDir := e.hub.Dir("app")
	if rng.IntN(2) == 0 {
		e.hub.Protect(t, "app", "main")
	}
	e.hub.AddIssue("app", "carol", "an issue", "body") // #1
	var ghLines []int64
	for _, b := range []string{"p1", "p2"} {
		mustGit(t, hubDir, "update-ref", "refs/heads/"+b, "refs/heads/main")
		e.hub.CommitFile(t, "app", b, b+".txt", "one\ntwo\n")
		n := e.hub.AddPR("app", "carol", b, "main", "PR from "+b, "Fixes #1")
		ghLines = append(ghLines, e.hub.AddReviewComment("app", n, "carol", b+".txt", 1, "from GitHub"))
	}
	alice, chris, aliceTok, _ := e.people(t)
	names := map[int64]string{1: "ben", alice: "alice", chris: "chris"}
	dev := e.clone(t, "app")
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	quick := func() { e.gh.HTTP.Timeout, e.m.FetchTimeout = 500*time.Millisecond, 3*time.Second }
	slow := func() { e.gh.HTTP.Timeout, e.m.FetchTimeout = 5*time.Second, 30*time.Second }
	opened := map[string]bool{} // branches with a PR opened on GitCove
	merges := 0

	// prs lists what the interface would offer: open mirrored PRs and new ones not yet on GitHub,
	// each with the branch its work is on.
	prs := func() (refs []string, heads map[string]string) {
		heads = map[string]string{}
		mirrored, _ := e.st.Issues(ctx, repoID)
		for _, i := range mirrored {
			if p, err := e.st.Pull(ctx, repoID, i.Number); err == nil && i.State == "open" {
				refs, heads[strconv.Itoa(i.Number)] = append(refs, strconv.Itoa(i.Number)), p.HeadRef
			}
		}
		for _, it := range e.metaItems(t) {
			if n, _, _ := e.st.MappedIssue(ctx, repoID, it.Issue); it.Kind == "issue" && it.Change.Head != "" && n == 0 && it.State != "skipped" {
				refs, heads[it.Issue] = append(refs, it.Issue), it.Change.Head
			}
		}
		return
	}

	outage := func() {
		e.hub.SetFaults(fakehub.Faults{})
		slow()
		if err := e.m.Cycle(ctx); err != nil {
			t.Fatal(err)
		}
		stop := func() {}
		if degraded {
			stop = e.degraded(t, rng)
		} else {
			e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
			e.mode(t, store.Failover)
		}
		for range 4 + rng.IntN(8) {
			refs, heads := prs()
			what, on := rng.IntN(7), ""
			if len(refs) == 0 {
				what = 0 // every PR is closed or merged: open one
			} else {
				on = pick(refs)
			}
			tok := []int64{1, alice, chris}[rng.IntN(3)]
			c := store.Change{Author: names[tok]}
			switch what {
			case 0: // push work and open a PR for it
				b := pick([]string{"d1", "d2", "d3"})
				work(t, dev, b, fmt.Sprintf("%s-%d.txt", b, rng.IntN(1e6)))
				if out, err := push(dev, b); err != nil && degraded && (!strings.Contains(out, "GitCove:") || strings.Contains(out, "cannot lock ref")) {
					continue // a sync moved the branch on GitCove meanwhile, before or just after the hook
				} else if err != nil {
					t.Fatalf("push %s: %s", b, out)
				}
				if !opened[b] {
					if st, got := e.post(t, e.tok, repo+"/pulls", url.Values{"head": {b}, "title": {"PR " + b}, "body": {"Fixes #1"}}); st != 303 {
						t.Fatalf("open PR from %s: %d %s", b, st, got)
					}
					opened[b] = true
				}
				continue
			case 1, 2: // review, sometimes with line comments
				c.Verdict = pick([]string{"comment", "approve", "approve", "request_changes"})
				c.Commit, c.Body = e.served(t, "app", "refs/heads/"+heads[on]), fmt.Sprintf("r%d", rng.IntN(1e6))
				for i := range rng.IntN(3) {
					c.Comments = append(c.Comments, store.LineComment{Path: "f.txt", Line: i + 1, Side: "RIGHT", Body: fmt.Sprintf("l%d", rng.IntN(1e6))})
				}
				e.rec(t, tok, "review", on, c)
			case 3: // reply to a line comment: GitHub's, or one in a review queued here
				var parents []string
				if n, err := strconv.Atoi(on); err == nil {
					lines, _ := e.st.ReviewComments(ctx, repoID, n)
					for _, l := range lines {
						if l.InReplyTo == 0 {
							parents = append(parents, fmt.Sprintf("c%d", l.ID))
						}
					}
				}
				for _, it := range e.metaItems(t) {
					if it.Kind == "review" && it.Issue == on && len(it.Change.Comments) > 0 && it.State != "done" {
						parents = append(parents, it.OpID+"/0")
					}
				}
				if len(parents) == 0 {
					continue
				}
				c.Parent, c.Body = pick(parents), fmt.Sprintf("y%d", rng.IntN(1e6))
				e.rec(t, tok, "reply", on, c)
			case 4: // approve as alice, then merge
				e.rec(t, alice, "review", on, store.Change{Author: "alice", Verdict: "approve", Commit: e.served(t, "app", "refs/heads/"+heads[on])})
				if st, _ := e.post(t, aliceTok, repo+"/issues/"+on+"/merge", nil); st == 303 {
					merges++
				}
			case 5:
				c.Body = fmt.Sprintf("c%d", rng.IntN(1e6))
				e.rec(t, tok, "comment", on, c)
			case 6: // meanwhile, on GitHub
				hubPRs := e.hub.PRs("app")
				pr := hubPRs[rng.IntN(len(hubPRs))]
				switch rng.IntN(6) {
				case 0:
					e.hub.Commit(t, "app", "main") // main moves
				case 1:
					if _, ok := e.hub.Refs(t, "app")["refs/heads/"+pr.Head]; ok {
						e.hub.Commit(t, "app", pr.Head) // the PR gets a commit: approvals of the old one are stale
					}
				case 2:
					e.hub.SetIssue("app", pr.Number, "state", "closed")
				case 3:
					if pr.State == "open" && pr.Author == "carol" {
						e.hub.MergePR(t, "app", pr.Number)
					}
				case 4:
					e.hub.DeleteReviewComment("app", ghLines[rng.IntN(len(ghLines))])
				case 5:
					if rng.IntN(3) == 0 {
						e.hub.RevokeUser("alice-gh")
					}
				}
			}
		}
		stop()
		if !degraded {
			e.hub.SetFaults(fakehub.Faults{})
			e.mode(t, store.Reconciling, store.Replaying)
		}
	}

	outage()
	outages, passes, kills := 1, 0, 0
	for ; passes < 30; passes++ {
		if m, _ := e.st.Mode(ctx); m == store.Normal {
			break
		}
		quick()
		e.hub.SetFaults(fakehub.Faults{
			APIDown:       rng.Float64() < 0.1,
			ErrorRate:     rng.Float64() * 0.2,
			TimeoutRate:   rng.Float64() * 0.1,
			LostRate:      rng.Float64() * 0.4,
			RateLimitRate: rng.Float64() * 0.1,
			ListingLag:    time.Duration(rng.Float64() * float64(300*time.Millisecond)),
		})
		r := *e.r
		r.Kill = func(string) error {
			if rng.Float64() < 0.15 {
				kills++
				return errKilled
			}
			return nil
		}
		_ = r.Pass(ctx)
		if outages < 3 && rng.IntN(4) == 0 {
			outage()
			outages++
		}
	}
	e.hub.SetFaults(fakehub.Faults{})
	slow()
	time.Sleep(e.r.Grace) // pushes recorded but never landed are settled after it
	for range 5 {
		if err := e.r.Pass(ctx); err != nil {
			t.Fatalf("clean pass: %v", err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}

	items := e.metaItems(t)
	byOp := map[string]store.Item{}
	states := map[string]int{}
	for _, it := range items {
		byOp[it.OpID] = it
		states[it.Kind+" "+it.State]++
	}
	t.Logf("%d outages, %d faulty passes, %d kills, %d local merges, items %v", outages, passes, kills, merges, states)

	refOps := map[string]bool{}
	for _, its := range e.items(t) {
		for _, it := range its {
			refOps[it.OpID] = true
		}
	}
	posted := map[string]int{}
	opOf := func(body string) string {
		_, after, ok := strings.Cut(body, "<!-- gitcove:op=")
		op, _, _ := strings.Cut(after, " -->")
		return map[bool]string{true: op}[ok]
	}
	// see counts a marker on GitHub and checks who posted it: a person only for their own item
	// and only if they authorised GitCove (R7).
	see := func(body, author, what string) store.Item {
		op := opOf(body)
		if op == "" {
			return store.Item{}
		}
		posted[op]++
		if refOps[op] { // a fallback PR for diverged pushes, posted by the App
			if posted[op] > 1 || author != fakehub.Bot {
				t.Errorf("fallback PR for push %s: copy %d, by %s", op, posted[op], author)
			}
			return store.Item{}
		}
		it, ok := byOp[op]
		if !ok {
			t.Errorf("marker %s on a %s belongs to no item", op, what)
		}
		if author != fakehub.Bot && (author != "alice-gh" || it.TokenID != alice) {
			t.Errorf("item %d by %s posted as %s", it.ID, names[it.TokenID], author)
		}
		return it
	}
	hubIssues := e.hub.Issues("app")
	for _, i := range hubIssues {
		see(i.FirstBody, i.Author, "issue")
	}
	for _, c := range e.hub.Comments("app") {
		see(c.Body, c.Author, "comment")
	}
	for _, c := range e.hub.ReviewComments("app") {
		see(c.Body, c.Author, "line comment")
	}
	for _, v := range e.hub.Reviews("app") {
		it := see(v.Body, v.Author, "review")
		// 5. No false approvals (R7).
		if v.State == "COMMENTED" {
			continue
		}
		want := map[string]string{"approve": "APPROVED", "request_changes": "CHANGES_REQUESTED"}[it.Change.Verdict]
		if v.Author != "alice-gh" || it.TokenID != alice || v.State != want || v.Commit != it.Change.Commit {
			t.Errorf("review %d on #%d is %s by %s at %.8s, from item %+v", v.ID, v.Number, v.State, v.Author, v.Commit, it)
		}
	}

	plan, err := e.r.IssuePlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waits, newIssue := map[int64]string{}, map[string]int64{}
	for _, s := range plan {
		waits[s.ID] = s.Waits
		if s.Kind == "issue" {
			newIssue["waits for "+s.Issue+", which is pending"] = s.ID
		}
	}
	// A change to a new issue or PR that waits for a human waits for that human too.
	for id, w := range waits {
		if l, ok := newIssue[w]; ok {
			waits[id] = waits[l]
		}
	}
	uncertainPosted := 0
	for _, it := range items {
		if it.State == "uncertain" && posted[it.OpID] > 0 {
			uncertainPosted++
		}
	}
	t.Logf("uncertain items already on GitHub (a blind retry would have duplicated them): %d", uncertainPosted)
	for _, it := range items {
		n := posted[it.OpID]
		silent := it.Kind == "label" || (it.Kind == "state" || it.Kind == "edit") && it.Target != "note" ||
			it.Kind == "merge" && strings.HasPrefix(it.Result, "already merged")
		switch {
		// 1. Nothing lost (R2).
		case it.State == "sending" || it.State == "pending" && waits[it.ID] == "":
			t.Errorf("item %d (%s on %s) ended %s", it.ID, it.Kind, it.Issue, it.State)
		case it.State == "pending" && it.Kind == "reply" && strings.HasSuffix(waits[it.ID], "review it replies to, which is pending"):
			// The review is checked itself: it waits for something that needs a human.
		case it.State == "pending" && !strings.HasSuffix(waits[it.ID], "uncertain") && !strings.HasSuffix(waits[it.ID], "conflict"):
			t.Errorf("item %d %s", it.ID, waits[it.ID])
		// 2. Nothing duplicated (R3).
		case n > 1:
			t.Errorf("item %d (%s) posted %d times", it.ID, it.Kind, n)
		case !silent && it.State == "done" && n != 1:
			t.Errorf("item %d (%s) done but on GitHub %d times: %s", it.ID, it.Kind, n, it.Result)
		case (it.State == "pending" || it.State == "conflict") && n != 0:
			t.Errorf("item %d %s but on GitHub", it.ID, it.State)
		}
	}

	// 3. GitHub's refs moved only by fast-forward, and replay never pushed to main (R1, R4).
	for _, p := range e.hub.Pushes("app") {
		if p.Ref == "refs/heads/main" {
			t.Errorf("replay pushed to main: %+v", p)
		}
		if strings.Trim(p.Old, "0") != "" {
			if out, err := git(hubDir, "merge-base", "--is-ancestor", p.Old, p.New); err != nil {
				t.Errorf("push to %s wasn't a fast-forward: %s", p.Ref, out)
			}
		}
	}

	// 6. A full sync agrees: main follows GitHub again unless something about it needs a human,
	// and the mirror has GitHub's PRs and line comments.
	held, err := e.st.HeldBranches(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if gh := e.hub.Refs(t, "app")["refs/heads/main"]; !held["main"] && e.served(t, "app", "refs/heads/main") != gh {
		t.Errorf("main on GitCove is %s, on GitHub %s", e.served(t, "app", "refs/heads/main"), gh)
	}
	for _, pr := range e.hub.PRs("app") {
		m, err := e.st.Pull(ctx, repoID, pr.Number)
		if err != nil || m.HeadRef != pr.Head || m.BaseRef != pr.Base || m.Merged != pr.Merged {
			t.Errorf("mirror of PR #%d = %+v (%v), GitHub has %+v", pr.Number, m, err, pr)
		}
	}
	mirrored := map[int64]bool{}
	for _, i := range hubIssues {
		lines, _ := e.st.ReviewComments(ctx, repoID, i.Number)
		for _, l := range lines {
			mirrored[l.ID] = true
		}
	}
	for _, c := range e.hub.ReviewComments("app") {
		if !mirrored[c.ID] {
			t.Errorf("line comment %d on #%d isn't mirrored", c.ID, c.Number)
		}
	}
}
