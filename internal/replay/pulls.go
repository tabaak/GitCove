package replay

// Replay of PRs opened on GitCove, reviews, replies to line comments and local merges (design
// 8.7 and 8.13). A review is posted as a review on the commit it was made on. An approval or a
// request for changes is applied only as the reviewer's own, with their token, and only while
// that commit is still the PR's head on GitHub (R7); otherwise it's told in words. A local merge
// never reaches GitHub as a push: the PR gets a note, and a person merges it there.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tabaak/gitcove/internal/store"
)

// waitsOnWork says why a new PR or a reply can't replay yet: a PR needs its branch on GitHub,
// and a reply to a queued review's line comment needs that review settled.
func (r *Replayer) waitsOnWork(ctx context.Context, it store.Item) (string, error) {
	switch {
	case it.Kind == "issue" && it.Change.Head != "":
		_, state, err := r.Store.PushedHead(ctx, it.RepoID, it.Change.Head)
		if err != nil || state == "" {
			return "", err
		}
		return fmt.Sprintf("waits for the push to %s, which is %s", it.Change.Head, state), nil
	case it.Kind == "reply" && !strings.HasPrefix(it.Change.Parent, "c"):
		review, _, err := r.Store.LocalComment(ctx, it.RepoID, it.Change.Parent)
		if err != nil || review.State == "done" || review.State == "skipped" {
			return "", err
		}
		return "waits for the review it replies to, which is " + review.State, nil
	}
	return "", nil
}

// parentID is the GitHub ID of the line comment a reply answers, 0 if it has none.
func (r *Replayer) parentID(ctx context.Context, it store.Item) (int64, error) {
	if id, ok := strings.CutPrefix(it.Change.Parent, "c"); ok {
		return strconv.ParseInt(id, 10, 64)
	}
	_, id, err := r.Store.LocalComment(ctx, it.RepoID, it.Change.Parent)
	return id, err
}

// quoteParent quotes the line comment a reply answers, for a reply posted as an ordinary comment.
func (r *Replayer) quoteParent(ctx context.Context, it store.Item) (string, error) {
	var author, path, body string
	var line int
	if id, err := r.parentID(ctx, it); err != nil {
		return "", err
	} else if strings.HasPrefix(it.Change.Parent, "c") {
		n, _ := strconv.Atoi(it.Issue)
		comments, err := r.Store.ReviewComments(ctx, it.RepoID, n)
		if err != nil {
			return "", err
		}
		for _, c := range comments {
			if c.ID == id {
				author, path, line, body = c.Author, c.Path, c.Line, c.Body
			}
		}
	} else {
		review, _, err := r.Store.LocalComment(ctx, it.RepoID, it.Change.Parent)
		if err != nil {
			return "", err
		}
		_, idx, _ := strings.Cut(it.Change.Parent, "/")
		if i, _ := strconv.Atoi(idx); i < len(review.Change.Comments) {
			c := review.Change.Comments[i]
			author, path, line, body = review.Change.Author, c.Path, c.Line, c.Body
		}
	}
	if body == "" {
		return "In reply to a line comment that is no longer on GitHub:\n\n", nil
	}
	return fmt.Sprintf("In reply to `%s` on `%s` line %d, a line comment that is no longer on GitHub:\n\n> %s\n\n",
		strings.ReplaceAll(author, "`", ""), path, line, strings.ReplaceAll(body, "\n", "\n> ")), nil
}

// reviewAction decides how a review goes up: "approve" applies its verdict, which takes the
// reviewer's own token and the PR's head on GitHub still being the commit they reviewed (R7);
// "review" posts it as a review that only comments and states the verdict in words.
func (r *Replayer) reviewAction(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, poster string) (string, error) {
	if it.Change.Verdict == "comment" || poster == "" {
		return "review", nil
	}
	pr, err := r.GH.PR(ctx, repo.Owner, repo.Name, number(it, nums))
	if refused(err) {
		return "review", nil // the write is refused the same way and says why
	} else if err != nil {
		return "", err
	}
	if pr.State == "open" && pr.Head.SHA == it.Change.Commit {
		return "approve", nil
	}
	return "review", nil
}

// reviewBody is a review's summary. Unless the verdict is applied as a real approval or change
// request, it says so in words. quote adds the line comments, for a review posted as an ordinary comment.
func reviewBody(it store.Item, quote bool) string {
	c := it.Change
	var b strings.Builder
	if verb := map[string]string{"approve": "Approved", "request_changes": "Requested changes"}[c.Verdict]; verb != "" && it.Target != "approve" {
		fmt.Fprintf(&b, "**%s on GitCove at commit `%.12s`. Not applied on GitHub**, where that takes the reviewer's own "+
			"authorisation and the PR still being at that commit.\n\n", verb, c.Commit)
	}
	if quote {
		fmt.Fprintf(&b, "A review of commit `%.12s`, which GitHub wouldn't take as a review of this PR, so its line comments are quoted here.\n\n", c.Commit)
	}
	b.WriteString(c.Body)
	if quote {
		for _, lc := range c.Comments {
			fmt.Fprintf(&b, "\n\n`%s` line %d:\n> %s", lc.Path, lc.Line, strings.ReplaceAll(lc.Body, "\n", "\n> "))
		}
	}
	return strings.TrimSpace(b.String())
}

// lineBody is a line comment as posted: the App names the person, since GitHub shows each line comment by itself.
func lineBody(it store.Item, lc store.LineComment, nums map[string]int, asUser bool) string {
	if asUser {
		return rewrite(lc.Body, nums)
	}
	return who(it) + ": " + rewrite(lc.Body, nums)
}

// mapComments records the GitHub IDs of a posted review's line comments, so replies to them
// can be posted as replies. It is best effort: listings can lag, and a reply whose parent has
// no ID is posted as an ordinary comment quoting it instead.
func (r *Replayer) mapComments(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int, reviewID int64) error {
	want := it.Change.Comments
	if len(want) == 0 {
		return nil
	}
	wait := r.MarkerWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	ids := map[int]int64{}
	for try := 0; try < 3 && len(ids) < len(want); try++ {
		if try > 0 {
			if err := sleepUntil(ctx, time.Now().Add(wait/2)); err != nil {
				return err
			}
		}
		got, err := r.GH.ReviewComments(ctx, repo.Owner, repo.Name, number(it, nums), reviewID, "")
		if err != nil {
			return err
		}
		used := map[int64]bool{}
		clear(ids)
		for i, lc := range want {
			for _, g := range got {
				if !used[g.ID] && g.Path == lc.Path && g.Body == lineBody(it, lc, nums, it.Poster != "") {
					used[g.ID], ids[i] = true, g.ID
					break
				}
			}
		}
	}
	return r.Store.MapReviewComments(ctx, it, ids)
}

// checkMerge settles a local merge that needs no note, and reports whether it did: the merge
// never completed on GitCove, GitHub already merged the PR, or GitHub closed it unmerged, which
// needs a human (design 8.7).
func (r *Replayer) checkMerge(ctx context.Context, repo store.Repo, it store.Item, nums map[string]int) (bool, error) {
	c := it.Change
	finish := func(state, result string) (bool, error) {
		return true, r.Store.Finish(ctx, []int64{it.ID}, state, result, local(it), "")
	}
	tip, err := r.Git.Ref(ctx, repo.GitHubID, "refs/heads/"+c.Base)
	if err != nil {
		return false, err
	}
	if in := tip != ""; in {
		if in, err = r.Git.IsAncestor(ctx, repo.GitHubID, c.Commit, tip); err != nil {
			return false, err
		} else if !in {
			return finish("conflict", "the merge was recorded but "+c.Base+" on GitCove doesn't contain it (GitCove stopped mid-merge). Skip this item and merge again")
		}
	}
	pr, err := r.GH.PR(ctx, repo.Owner, repo.Name, number(it, nums))
	switch {
	case refused(err):
		return finish("conflict", "GitHub can't show the PR: "+err.Error())
	case err != nil:
		return false, err
	case pr.MergedAt != nil:
		return finish("done", "already merged on GitHub")
	case pr.State != "open":
		if r.Alert != nil {
			r.Alert(fmt.Sprintf("%s #%d was merged on GitCove but closed on GitHub without merging", repo.FullName(), pr.Number))
		}
		return finish("conflict", fmt.Sprintf("the PR was closed on GitHub without merging. %s on GitCove keeps the merge until you skip this item", c.Base))
	}
	return false, nil
}

// duplicate names an issue or PR opened on GitHub, after a new one was queued here, with the
// same title: probably someone re-filing it by hand (design 8.8). A hint for the admin only.
func (r *Replayer) duplicate(ctx context.Context, it store.Item) (string, error) {
	if it.Kind != "issue" {
		return "", nil
	}
	mirrored, err := r.Store.Issues(ctx, it.RepoID)
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	for _, i := range mirrored {
		if i.CreatedAt > it.Created && norm(i.Title) == norm(it.Change.Title) && !strings.Contains(i.Body, "<!-- gitcove:op=") {
			return fmt.Sprintf("#%d", i.Number), nil
		}
	}
	return "", err
}
