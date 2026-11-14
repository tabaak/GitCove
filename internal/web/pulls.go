package web

// PRs in the interface (design 8.7 and 8.13): the branch list, the diff a PR or a comparison
// shows, opening a PR, reviewing with line comments, replying to them, and merging locally.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/store"
)

type diffLine struct {
	Kind     string // "+", "-", " " or "@" for a hunk header
	Old, New int    // line numbers, 0 where the line doesn't exist
	Text     string
	Field    string // form field name for a comment on this line, "" if it can't take one
}

type diffFile struct {
	Path  string
	Lines []diffLine
}

type diffView struct {
	Files     []diffFile
	Truncated bool
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)

// maxDiffLines keeps a huge diff from becoming a huge page; the rest is a `git diff` away.
const maxDiffLines = 5000

// parseDiff reads git's unified diff. A line can take a review comment on the side GitHub uses:
// RIGHT for added and unchanged lines, LEFT for removed ones.
func parseDiff(text string) diffView {
	var v diffView
	var f *diffFile
	var oldPath string
	old, cur, n := 0, 0, 0
	inHunk := false
	for _, l := range strings.Split(text, "\n") {
		if n >= maxDiffLines {
			v.Truncated = true
			break
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			v.Files = append(v.Files, diffFile{Path: strings.TrimPrefix(l[strings.LastIndex(l, " b/")+1:], "b/")})
			f, inHunk = &v.Files[len(v.Files)-1], false
		case f == nil:
		case !inHunk && strings.HasPrefix(l, "--- "):
			oldPath = strings.TrimPrefix(strings.TrimPrefix(l, "--- "), "a/")
		case !inHunk && strings.HasPrefix(l, "+++ "):
			if f.Path = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/"); f.Path == "/dev/null" {
				f.Path = oldPath
			}
		case strings.HasPrefix(l, "@@"):
			if m := hunkRe.FindStringSubmatch(l); m != nil {
				old, _ = strconv.Atoi(m[1])
				cur, _ = strconv.Atoi(m[2])
				inHunk = true
				f.Lines = append(f.Lines, diffLine{Kind: "@", Text: l})
			}
		case !inHunk:
			if strings.HasPrefix(l, "Binary files") {
				f.Lines = append(f.Lines, diffLine{Kind: "@", Text: "binary file"})
			}
		case strings.HasPrefix(l, "+"):
			f.Lines = append(f.Lines, diffLine{Kind: "+", New: cur, Text: l[1:], Field: fmt.Sprintf("c|RIGHT|%d|%s", cur, f.Path)})
			cur++
			n++
		case strings.HasPrefix(l, "-"):
			f.Lines = append(f.Lines, diffLine{Kind: "-", Old: old, Text: l[1:], Field: fmt.Sprintf("c|LEFT|%d|%s", old, f.Path)})
			old++
			n++
		case strings.HasPrefix(l, " "):
			f.Lines = append(f.Lines, diffLine{Kind: " ", Old: old, New: cur, Text: l[1:], Field: fmt.Sprintf("c|RIGHT|%d|%s", cur, f.Path)})
			old++
			cur++
			n++
		}
	}
	return v
}

// reviewView is a review's summary: GitHub's, or one queued here.
type reviewView struct{ Author, Verdict, Body, At, Badge, Commit string }

// lineThread is a line comment with its replies. Parent is what a reply to it names.
type lineThread struct {
	Parent, Path string
	Line         int
	Posts        []post
}

// prView is what a PR's page shows beyond an issue's.
type prView struct {
	Head, Base   string
	HeadSHA      string // the commit GitCove would merge, "" if it doesn't have the branch
	Fork, Merged bool   // Merged: on GitHub
	MergedLocal  string // who merged it on GitCove and how far that got, "" if nobody did
	Approvals    []string
	Needed       int
	Mergeable    bool
	Why          string // why it can't be merged here yet
	Author       bool   // the viewer wrote the PR, so can't approve it
	Reviews      []reviewView
	Threads      []lineThread
}

func verdictText(v string) string {
	return map[string]string{"approve": "approved", "APPROVED": "approved", "request_changes": "requested changes",
		"CHANGES_REQUESTED": "requested changes", "DISMISSED": "review dismissed"}[v]
}

// fillPR completes th.PR for a PR: one mirrored from GitHub, or one opened here (th.PR then
// already has its branches). items are the repo's unsettled items, oldest first.
func (s *Server) fillPR(ctx context.Context, p *page, th *thread, items []store.Item) error {
	pr := th.PR
	n, _ := strconv.Atoi(th.Ref)
	if pr == nil {
		m, err := s.Store.Pull(ctx, p.Repo.ID, n)
		if errors.Is(err, store.ErrNotFound) {
			return nil // not synced as a PR yet: shown as its conversation only
		} else if err != nil {
			return err
		}
		pr = &prView{Head: m.HeadRef, Base: m.BaseRef, Fork: m.Fork, Merged: m.Merged}
		th.PR = pr
	}
	pr.Needed = p.Repo.Approvals
	pr.Author = p.User.Name == th.Author || p.User.GitHub != "" && p.User.GitHub == th.Author
	var err error
	if !pr.Fork {
		if pr.HeadSHA, err = s.Git.Ref(ctx, p.Repo.GitHubID, "refs/heads/"+pr.Head); err != nil {
			return err
		}
	}
	if pr.HeadSHA == "" && !th.Local {
		if pr.HeadSHA, err = s.Git.Ref(ctx, p.Repo.GitHubID, fmt.Sprintf("refs/pull/%d/head", n)); err != nil {
			return err
		}
	}
	threads := map[string]*lineThread{}
	var order []string
	add := func(key, path string, line int, po post) {
		if threads[key] == nil {
			threads[key] = &lineThread{Parent: key, Path: path, Line: line}
			order = append(order, key)
		}
		threads[key].Posts = append(threads[key].Posts, po)
	}
	if !th.Local {
		reviews, err := s.Store.Reviews(ctx, p.Repo.ID, n)
		if err != nil {
			return err
		}
		for _, v := range reviews {
			if v.State == "APPROVED" && v.Commit == pr.HeadSHA && v.Author != th.Author && !slices.Contains(pr.Approvals, v.Author+" (on GitHub)") {
				pr.Approvals = append(pr.Approvals, v.Author+" (on GitHub)")
			}
			if v.Body != "" || v.State != "COMMENTED" {
				pr.Reviews = append(pr.Reviews, reviewView{v.Author, verdictText(v.State), v.Body, v.SubmittedAt, "", v.Commit})
			}
		}
		comments, err := s.Store.ReviewComments(ctx, p.Repo.ID, n)
		if err != nil {
			return err
		}
		for _, c := range comments {
			root := c.ID
			if c.InReplyTo != 0 {
				root = c.InReplyTo
			}
			add(fmt.Sprintf("c%d", root), c.Path, c.Line, post{c.Author, c.Body, c.CreatedAt, ""})
		}
	}
	toks, err := s.Store.Tokens(ctx)
	if err != nil {
		return err
	}
	github := map[int64]string{} // the GitHub login each token's holder authorised, to spot the PR's author
	for _, t := range toks {
		github[t.ID] = t.GitHub
	}
	for _, it := range items {
		c := it.Change
		switch {
		case it.Issue != th.Ref:
		case it.Kind == "review":
			badge := status(it.State)
			if c.Verdict == "approve" {
				badge += ", not checked by GitHub"
				if c.Commit == pr.HeadSHA && c.Author != th.Author && cmp.Or(github[it.TokenID], c.Author) != th.Author && !slices.Contains(pr.Approvals, c.Author) {
					pr.Approvals = append(pr.Approvals, c.Author)
				}
			}
			pr.Reviews = append(pr.Reviews, reviewView{c.Author, verdictText(c.Verdict), c.Body, it.Created, badge, c.Commit})
			for i, lc := range c.Comments {
				add(fmt.Sprintf("%s/%d", it.OpID, i), lc.Path, lc.Line, post{c.Author, lc.Body, it.Created, status(it.State)})
			}
		case it.Kind == "reply" && threads[c.Parent] != nil:
			add(c.Parent, "", 0, post{c.Author, c.Body, it.Created, status(it.State)})
		}
	}
	for _, k := range order {
		pr.Threads = append(pr.Threads, *threads[k])
	}
	merges, err := s.Store.MetaItems(ctx, p.Repo.ID, "pending", "sending", "conflict", "uncertain", "done")
	if err != nil {
		return err
	}
	for _, it := range merges {
		if it.Kind == "merge" && it.Issue == th.Ref {
			pr.MergedLocal = fmt.Sprintf("Merged into %s on GitCove by %s", it.Change.Base, it.Change.Author)
			if it.State != "done" {
				pr.MergedLocal += ", " + status(it.State)
			}
		}
	}
	switch {
	case pr.Merged:
		pr.Why = "merged on GitHub"
	case pr.MergedLocal != "":
		pr.Why = "already merged on GitCove"
	case th.State != "open":
		pr.Why = "the PR is closed"
	case pr.Fork:
		pr.Why = "it comes from a fork, whose branch GitCove doesn't have. Forks can be read, commented on and reviewed"
	case pr.HeadSHA == "":
		pr.Why = "GitCove doesn't have the branch " + pr.Head
	case len(pr.Approvals) < pr.Needed:
		pr.Why = fmt.Sprintf("it needs %d approval(s) of its current commit from someone other than its author, and has %d", pr.Needed, len(pr.Approvals))
	default:
		pr.Mergeable = true
	}
	return nil
}

func (s *Server) branches(w http.ResponseWriter, r *http.Request, p *page) error {
	list, err := s.Git.Branches(r.Context(), p.Repo.GitHubID)
	if err != nil {
		return err
	}
	p.Data = list
	return s.render(w, "branches", p)
}

// compare shows what head would change in base, and during failover offers to open a PR for it.
func (s *Server) compare(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	base, head := r.URL.Query().Get("base"), r.URL.Query().Get("head")
	if base == "" {
		base = p.Repo.DefaultBranch
	}
	diff, err := s.branchDiff(ctx, p.Repo, base, head)
	if err != nil {
		return err
	}
	p.Data = struct {
		Base, Head string
		Diff       diffView
	}{base, head, diff}
	return s.render(w, "compare", p)
}

// branchDiff diffs two served branches; a missing one is a 404.
func (s *Server) branchDiff(ctx context.Context, repo store.Repo, base, head string) (diffView, error) {
	for _, b := range []string{base, head} {
		if sha, err := s.Git.Ref(ctx, repo.GitHubID, "refs/heads/"+b); err != nil {
			return diffView{}, err
		} else if sha == "" {
			return diffView{}, store.ErrNotFound
		}
	}
	text, err := s.Git.Diff(ctx, repo.GitHubID, "refs/heads/"+base, "refs/heads/"+head)
	return parseDiff(text), err
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	title, err := text(r, "title", true, 256)
	if err != nil {
		return err
	}
	body, err := text(r, "body", false, maxBody)
	if err != nil {
		return err
	}
	base, head := r.FormValue("base"), r.FormValue("head")
	if base == "" {
		base = p.Repo.DefaultBranch
	}
	diff, err := s.branchDiff(ctx, p.Repo, base, head)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest("GitCove has no such branch; push it first")
	} else if err != nil {
		return err
	}
	if base == head || len(diff.Files) == 0 {
		return badRequest(fmt.Sprintf("%s has nothing that %s lacks", head, base))
	}
	ref, err := s.Store.Record(ctx, p.Repo.ID, p.User.ID, "issue", "", store.Change{Author: p.User.Name, Title: title, Body: body, Head: head, Base: base})
	if err != nil {
		return err
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/issues/%s", p.Repo.FullName(), ref), http.StatusSeeOther)
	return nil
}

// prDiff is the diff of the PR on th's page: its head against its base.
func (s *Server) prDiff(ctx context.Context, repo store.Repo, th thread) (diffView, error) {
	if th.PR == nil || th.PR.HeadSHA == "" {
		return diffView{}, store.ErrNotFound
	}
	text, err := s.Git.Diff(ctx, repo.GitHubID, "refs/heads/"+th.PR.Base, th.PR.HeadSHA)
	var ge *gitstore.GitError
	if errors.As(err, &ge) {
		return diffView{}, badRequest("GitCove can't compare these: it lacks the branch " + th.PR.Base)
	}
	return parseDiff(text), err
}

func (s *Server) files(w http.ResponseWriter, r *http.Request, p *page) error {
	th, n, err := s.thread(r, p, r.PathValue("ref"))
	if err != nil {
		return err
	}
	if n != 0 {
		http.Redirect(w, r, fmt.Sprintf("/%s/issues/%d/files", p.Repo.FullName(), n), http.StatusSeeOther)
		return nil
	}
	diff, err := s.prDiff(r.Context(), p.Repo, th)
	if err != nil {
		return err
	}
	p.Data = struct {
		thread
		Diff diffView
	}{th, diff}
	return s.render(w, "files", p)
}

// review reads a review from the files page's form: a verdict, a summary, and a comment for
// each line whose box was filled in. The lines must be in the diff of the commit reviewed.
func (s *Server) review(r *http.Request, p *page, th thread) (store.Change, error) {
	c := store.Change{Author: p.User.Name, Verdict: r.FormValue("verdict"), Commit: r.FormValue("commit")}
	if th.PR == nil {
		return c, store.ErrNotFound
	}
	if c.Commit != th.PR.HeadSHA {
		return c, badRequest("the PR has new commits since you opened the page. Reload it and review those")
	}
	var err error
	if c.Body, err = text(r, "body", false, maxBody); err != nil {
		return c, err
	}
	diff, err := s.prDiff(r.Context(), p.Repo, th)
	if err != nil {
		return c, err
	}
	for _, f := range diff.Files {
		for _, l := range f.Lines {
			if l.Field == "" || strings.TrimSpace(r.FormValue(l.Field)) == "" {
				continue
			}
			body, err := text(r, l.Field, true, maxBody)
			if err != nil {
				return c, err
			}
			part := strings.SplitN(l.Field, "|", 4)
			line, _ := strconv.Atoi(part[2])
			c.Comments = append(c.Comments, store.LineComment{Path: f.Path, Line: line, Side: part[1], Body: body})
		}
	}
	switch c.Verdict {
	case "comment":
		if c.Body == "" && len(c.Comments) == 0 {
			return c, badRequest("a review needs a summary or a line comment")
		}
	case "approve", "request_changes":
		if th.PR.Author {
			return c, badRequest("you can't approve or request changes on your own PR")
		}
	default:
		return c, badRequest("verdict must be comment, approve or request_changes")
	}
	return c, nil
}

// closing finds "fixes #12" and the like: issues GitHub closes when the PR is merged.
var closing = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?\s+#(L-\d+|\d+)\b`)

// merge merges th's PR into its base on GitCove with a merge commit and queues a note for the PR
// on GitHub, where a person merges it again (design 8.7). A conflict changes nothing.
func (s *Server) merge(r *http.Request, p *page, th thread) error {
	ctx := r.Context()
	pr := th.PR
	if pr == nil {
		return store.ErrNotFound
	}
	if !pr.Mergeable {
		return badRequest("this PR can't be merged on GitCove: " + pr.Why)
	}
	base, err := s.Git.Ref(ctx, p.Repo.GitHubID, "refs/heads/"+pr.Base)
	if err != nil {
		return err
	} else if base == "" {
		return badRequest("GitCove doesn't have the branch " + pr.Base)
	}
	if in, err := s.Git.IsAncestor(ctx, p.Repo.GitHubID, pr.HeadSHA, base); err != nil {
		return err
	} else if in {
		return badRequest(pr.Base + " already has everything in " + pr.Head)
	}
	num := th.Ref
	if !th.Local {
		num = "#" + th.Ref
	}
	commit, err := s.Git.MergeCommit(ctx, p.Repo.GitHubID, base, pr.HeadSHA, p.User.Name,
		fmt.Sprintf("Merge %s from %s on GitCove\n\n%s", num, pr.Head, th.Title))
	if errors.Is(err, gitstore.ErrMergeConflict) {
		return badRequest(fmt.Sprintf("%s conflicts with %s, so nothing was merged. Merge %s into %s (or rebase), push, and get the new commit approved",
			pr.Head, pr.Base, pr.Base, pr.Head))
	} else if err != nil {
		return err
	}
	items, err := s.Store.MetaItems(ctx, p.Repo.ID, "pending", "sending", "conflict", "uncertain")
	if err != nil {
		return err
	}
	var earlier []string
	for _, it := range items {
		if it.Kind == "merge" && it.Change.Base == pr.Base {
			earlier = append(earlier, "#"+it.Issue) // #L-n is rewritten to the PR's number when posted
		}
	}
	note := fmt.Sprintf("Merged into `%s` on GitCove at commit `%.12s`, approved there by %s. GitCove never pushes a merge to GitHub: "+
		"merge this PR here once its checks pass.", pr.Base, pr.HeadSHA, "`"+strings.Join(pr.Approvals, "`, `")+"`")
	if len(earlier) > 0 {
		note += fmt.Sprintf(" It was merged after %s, so merge those first.", strings.Join(earlier, ", "))
	}
	var closes []string
	for _, m := range closing.FindAllStringSubmatch(th.Title+"\n"+th.Body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			if i, err := s.Store.Issue(ctx, p.Repo.ID, n); err != nil || i.State != "open" || i.IsPR {
				continue
			}
		} else if _, state, err := s.Store.MappedIssue(ctx, p.Repo.ID, m[1]); err != nil || state == "" || state == "skipped" {
			continue
		}
		if m[1] != th.Ref && !slices.Contains(closes, m[1]) {
			closes = append(closes, m[1])
		}
	}
	ids, err := s.Store.RecordMerge(ctx, p.Repo.ID, p.User.ID, th.Ref,
		store.Change{Author: p.User.Name, Head: pr.Head, Base: pr.Base, Commit: commit, Body: note}, closes)
	if err != nil {
		return err
	}
	// Recorded first, so a crash can't leave a merge that replay doesn't know of. If the branch
	// moved meanwhile, the record is taken back and nothing was merged.
	if err := s.Git.MoveBranch(ctx, p.Repo.GitHubID, pr.Base, commit, base); err != nil {
		if uerr := s.Store.Unrecord(ctx, ids); uerr != nil {
			return uerr
		}
		return badRequest(pr.Base + " changed while merging. Try again")
	}
	return nil
}
