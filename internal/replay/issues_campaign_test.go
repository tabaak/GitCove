package replay

import (
	"context"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tabaak/gitcove/fakehub"
	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/store"
)

// TestIssueCampaign runs seeded failovers in which two people, one of whom authorised GitCove,
// open issues, comment, label, close, reopen and edit, while GitHub changes the same issues.
// Replay then runs with faults: 500s, timeouts, lost responses, rate limits, listings that lag
// behind writes by more than the marker wait, kills at each crash point, and repeat outages.
// Afterwards it checks R2, R3, R5 and R7 and that a full sync agrees (plan section 3, checks 1,
// 2, 4, 5 and 6). COVE_SEEDS sets the number of seeds (default 10; the nightly run uses 200).
// Repeat one with: COVE_SEEDS=N go test -run 'TestIssueCampaign/seed=K$' ./internal/replay
func TestIssueCampaign(t *testing.T) {
	seeds := 10
	if s, err := strconv.Atoi(os.Getenv("COVE_SEEDS")); err == nil {
		seeds = s
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			issueCampaign(t, seed)
		})
	}
}

func issueCampaign(t *testing.T, seed uint64) {
	ctx := context.Background()
	e := setup(t, seed, "app")
	rng := mrand.New(mrand.NewPCG(seed, 11))
	repoID := e.repoID(t)
	for i := range 3 {
		e.hub.AddIssue("app", "carol", fmt.Sprintf("issue %d", i+1), "body")
	}
	alice, _, err := e.st.CreateToken(ctx, "test", "alice", []string{e.hub.Org + "/app"})
	if err != nil {
		t.Fatal(err)
	}
	users, err := auth.New(e.gh, e.st, slog.New(slog.DiscardHandler), []byte(strings.Repeat("cd", 32)))
	if err != nil {
		t.Fatal(err)
	}
	e.r.Users = users
	if _, err := users.Authorise(ctx, alice, "test", func(code, _ string) { e.hub.ApproveDevice(code, "alice-gh") }); err != nil {
		t.Fatal(err)
	}
	names := map[int64]string{1: "ben", alice: "alice"}
	var locals []string
	ghSide := map[string]string{} // "n/field" -> the last value GitHub's side set, for the R5 check
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	ref := func() string { // sometimes mention a new issue in text
		if len(locals) > 0 && rng.IntN(3) == 0 {
			return " see #" + pick(locals)
		}
		return ""
	}
	quick := func() { e.gh.HTTP.Timeout = 500 * time.Millisecond }
	slow := func() { e.gh.HTTP.Timeout = 5 * time.Second }

	outage := func() {
		e.hub.SetFaults(fakehub.Faults{})
		slow()
		if err := e.m.Cycle(ctx); err != nil { // the regular sync, before GitHub fails
			t.Fatal(err)
		}
		e.hub.SetFaults(fakehub.Faults{APIDown: true, GitDown: true})
		e.mode(t, store.Failover)
		for range 3 + rng.IntN(8) {
			var targets []string // what the web interface would offer: synced issues, and new ones not yet on GitHub
			mirrored, _ := e.st.Issues(ctx, repoID)
			for _, i := range mirrored {
				targets = append(targets, strconv.Itoa(i.Number))
			}
			for _, l := range locals {
				if n, _, _ := e.st.MappedIssue(ctx, repoID, l); n == 0 {
					targets = append(targets, l)
				}
			}
			tok := []int64{1, alice}[rng.IntN(2)]
			c := store.Change{Author: names[tok]}
			on := pick(targets)
			kind := pick([]string{"issue", "comment", "comment", "label", "state", "edit"})
			switch kind {
			case "issue":
				c.Title, c.Body = fmt.Sprintf("t%d", rng.IntN(1e6)), "b"+ref()
				if rng.IntN(3) == 0 {
					c.Labels = []string{"bug"}
				}
			case "comment":
				c.Body = fmt.Sprintf("c%d%s", rng.IntN(1e6), ref())
			case "label":
				c.Label, c.Add = pick([]string{"bug", "enhancement"}), rng.IntN(2) == 0
			case "state":
				c.State = pick([]string{"open", "closed"})
			case "edit":
				c.Field, c.Value = pick([]string{"title", "body"}), fmt.Sprintf("e%d%s", rng.IntN(1e6), ref())
				if strings.HasPrefix(on, "L-") { // a new issue is edited in place while it waits
					_ = e.st.EditLocal(ctx, repoID, on, "edited "+on, c.Value) // refused once it's replaying
					continue
				}
			}
			l, err := e.st.Record(ctx, repoID, tok, kind, on, c)
			if err != nil {
				t.Fatalf("record %s on %s: %v", kind, on, err)
			}
			if kind == "issue" {
				locals = append(locals, l)
			}
			if rng.IntN(3) == 0 { // meanwhile, on GitHub
				hubIssues := e.hub.Issues("app")
				n := 1 + rng.IntN(len(hubIssues))
				switch rng.IntN(4) {
				case 0, 1:
					f := pick([]string{"title", "body", "state"})
					v := fmt.Sprintf("gh%d", rng.IntN(1e6))
					if f == "state" {
						v = pick([]string{"open", "closed"})
					}
					e.hub.SetIssue("app", n, f, v)
					ghSide[fmt.Sprintf("%d/%s", n, f)] = v
				case 2:
					e.hub.AddComment("app", n, "carol", "from GitHub")
				case 3:
					e.hub.AddIssue("app", "carol", "filed on GitHub", "body")
				}
			}
		}
		e.hub.SetFaults(fakehub.Faults{})
		e.mode(t, store.Reconciling, store.Replaying)
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
			ListingLag:    time.Duration(rng.Float64() * float64(300*time.Millisecond)), // the marker wait is 100ms
		})
		r := *e.r // a fresh Replayer each pass, as after a restart
		r.Kill = func(string) error {
			if rng.Float64() < 0.15 {
				kills++
				return errKilled
			}
			return nil
		}
		_ = r.Pass(ctx) // errors expected
		if outages < 3 && rng.IntN(4) == 0 {
			outage() // GitHub fails again mid-replay; more work joins the queue
			outages++
		}
	}
	e.hub.SetFaults(fakehub.Faults{})
	slow()
	for range 5 {
		if err := e.r.Pass(ctx); err != nil {
			t.Fatalf("clean pass: %v", err)
		}
	}
	time.Sleep(300 * time.Millisecond) // the longest listing lag, so the sync below sees everything
	if err := e.m.Cycle(ctx); err != nil {
		t.Fatal(err)
	}

	items := e.metaItems(t)
	byOp := map[string]store.Item{}
	states := map[string]int{}
	for _, it := range items {
		byOp[it.OpID] = it
		states[it.State]++
	}
	t.Logf("%d outages, %d faulty passes, %d kills, items by state %v", outages, passes, kills, states)

	// Where each marker appears on GitHub, and who posted it.
	posted := map[string][]string{} // op -> authors
	texts := map[string]string{}
	opOf := func(body string) string {
		_, after, ok := strings.Cut(body, "<!-- gitcove:op=")
		op, _, _ := strings.Cut(after, " -->")
		return map[bool]string{true: op}[ok]
	}
	hubIssues, hubComments := e.hub.Issues("app"), e.hub.Comments("app")
	for _, i := range hubIssues {
		if op := opOf(i.FirstBody); op != "" { // as created: a later edit may replace the body
			posted[op] = append(posted[op], i.Author)
			texts[op] = i.FirstBody
		}
	}
	for _, c := range hubComments {
		if op := opOf(c.Body); op != "" {
			posted[op] = append(posted[op], c.Author)
			texts[op] = c.Body
		}
	}
	plan, err := e.r.IssuePlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waits := map[int64]string{}
	for _, s := range plan {
		waits[s.ID] = s.Waits
	}

	for _, it := range items {
		n := len(posted[it.OpID])
		text := it.Kind == "issue" || it.Kind == "comment" || it.Target == "note"
		switch {
		// 1. Nothing lost (R2): every item is final, or waits on one that needs a human.
		case it.State == "sending" || it.State == "pending" && waits[it.ID] == "":
			t.Errorf("item %d (%s on %s) ended %s", it.ID, it.Kind, it.Issue, it.State)
		case it.State == "pending" && !strings.HasSuffix(waits[it.ID], "uncertain") && !strings.HasSuffix(waits[it.ID], "conflict"):
			t.Errorf("item %d %s", it.ID, waits[it.ID])
		// 2. Nothing duplicated (R3): a text item is on GitHub once if done, at most once if
		// uncertain, and never if it was refused or never sent.
		case n > 1:
			t.Errorf("item %d (%s) posted %d times", it.ID, it.Kind, n)
		case text && it.State == "done" && n != 1:
			t.Errorf("item %d (%s) done but on GitHub %d times: %s", it.ID, it.Kind, n, it.Result)
		case (it.State == "pending" || it.State == "conflict") && n != 0:
			t.Errorf("item %d %s but on GitHub", it.ID, it.State)
		}
		// 5. Posted as a person only if that person authorised it (R7), else with attribution.
		for _, a := range posted[it.OpID] {
			switch {
			case a == "alice-gh" && it.TokenID != alice, a != "alice-gh" && a != fakehub.Bot:
				t.Errorf("item %d by %s posted as %s", it.ID, names[it.TokenID], a)
			case a == fakehub.Bot && it.Target != "note" && !strings.HasPrefix(texts[it.OpID], "Originally by `"+names[it.TokenID]+"`"):
				t.Errorf("item %d posted by the App without attribution: %q", it.ID, texts[it.OpID])
			}
		}
	}
	for op := range posted {
		if _, ok := byOp[op]; !ok {
			t.Errorf("marker %s on GitHub belongs to no item", op)
		}
	}

	// 4. Nothing newer overwritten (R5): a value GitHub's side set is gone only if replay applied
	// a change made against exactly that value.
	for key, v := range ghSide {
		ns, field, _ := strings.Cut(key, "/")
		n, _ := strconv.Atoi(ns)
		i := hubIssues[n-1]
		final := map[string]string{"title": i.Title, "body": i.Body, "state": i.State}[field]
		if final == v {
			continue
		}
		ok := false
		for _, it := range items {
			on := number(it, map[string]int{})
			if strings.HasPrefix(it.Issue, "L-") {
				on, _, _ = e.st.MappedIssue(ctx, repoID, it.Issue)
			}
			kindOK := field == "state" && it.Kind == "state" || it.Kind == "edit" && it.Change.Field == field
			ok = ok || on == n && kindOK && it.State == "done" && it.Base == v // applied, or applied before a lost answer
		}
		if !ok {
			t.Errorf("#%d %s was %q on GitHub, replay left %q", n, field, v, final)
		}
	}

	// 6. A full sync agrees with GitHub.
	mirrored, _ := e.st.Issues(ctx, repoID)
	if len(mirrored) != len(hubIssues) {
		t.Errorf("mirror has %d issues, GitHub %d", len(mirrored), len(hubIssues))
	}
	for _, m := range mirrored {
		g := hubIssues[m.Number-1]
		slices.Sort(m.Labels)
		gl := slices.Sorted(slices.Values(g.Labels))
		if m.Title != g.Title || m.Body != g.Body || m.State != g.State || !slices.Equal(m.Labels, gl) {
			t.Errorf("mirror #%d = %+v, GitHub has %+v", m.Number, m, g)
		}
	}
	var mirroredComments int
	for _, i := range hubIssues {
		cs, _ := e.st.Comments(ctx, repoID, i.Number)
		mirroredComments += len(cs)
	}
	if mirroredComments != len(hubComments) {
		t.Errorf("mirror has %d comments, GitHub %d", mirroredComments, len(hubComments))
	}
}
