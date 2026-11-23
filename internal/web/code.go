package web

// The Code tab, laid out like GitHub's: a branch's files and README, a file, a branch's commits,
// and a commit. Read-only, shown from the branches GitCove serves.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/store"
)

// maxFile is the largest file shown; bigger ones are a `git show` away.
const maxFile = 1 << 20

const commitsPerPage = 35

// pathEsc escapes each segment of a path for a URL, so names with # or ? still link.
func pathEsc(p string) string {
	seg := strings.Split(p, "/")
	for i := range seg {
		seg[i] = url.PathEscape(seg[i])
	}
	return strings.Join(seg, "/")
}

// size shows a byte count as GitHub does, such as "2.31 KB".
func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d Bytes", n)
	}
	f, unit := float64(n)/1024, "KB"
	if f >= 1024 {
		f, unit = f/1024, "MB"
	}
	return fmt.Sprintf("%.3g %s", f, unit)
}

var noreply = regexp.MustCompile(`^(?:\d+\+)?([A-Za-z0-9-]+)@users\.noreply\.github\.com$`)

// ghLogin finds the GitHub login in a commit email, for the avatar; "" for other emails.
func ghLogin(email string) string {
	if m := noreply.FindStringSubmatch(email); m != nil {
		return m[1]
	}
	return ""
}

// crumb is one step of a path, linking to that directory.
type crumb struct{ Name, Path string }

// spot is where in a repo a page is: a served branch, a path in it, and the other branches.
type spot struct {
	Branch, Path string
	Parent       string // the directory holding Path
	Crumbs       []crumb
	Branches     []string
}

// locate splits rest, such as "feature/x/docs/a.md", into a served branch and a path in it. Only
// one branch can fit: Git refuses a branch "a" beside "a/b".
func (s *Server) locate(ctx context.Context, p *page, rest string) (spot, error) {
	list, err := s.Git.Branches(ctx, p.Repo.GitHubID)
	if err != nil {
		return spot{}, err
	}
	var sp spot
	for _, b := range list {
		sp.Branches = append(sp.Branches, b.Name)
		if rest == b.Name || strings.HasPrefix(rest, b.Name+"/") {
			sp.Branch = b.Name
		}
	}
	if sp.Branch == "" {
		return sp, store.ErrNotFound
	}
	if sp.Path = strings.TrimPrefix(rest[len(sp.Branch):], "/"); sp.Path != "" {
		for i, name := range strings.Split(sp.Path, "/") { // ServeMux has already cleaned out "." and ".."
			sp.Crumbs = append(sp.Crumbs, crumb{name, strings.Join(strings.Split(sp.Path, "/")[:i+1], "/")})
		}
		if len(sp.Crumbs) > 1 {
			sp.Parent = sp.Crumbs[len(sp.Crumbs)-2].Path
		}
	}
	return sp, nil
}

// browse shows a directory with its README, or a file. Both /tree/ and /blob/ come here, as does
// the repo's own page, which is the default branch's top directory.
func (s *Server) browse(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	rest := r.PathValue("rest")
	if rest == "" {
		rest = p.Repo.DefaultBranch
	}
	sp, err := s.locate(ctx, p, rest)
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	v := struct {
		spot
		CloneURL string
		Last     *gitstore.Commit // the newest commit touching Path
		Commits  int
		Entries  []gitstore.Entry
		Readme   *gitstore.Entry
		Text     string
		Lines    []template.HTML // coloured
		Markdown bool            // a Markdown file, shown rendered unless ?plain=1
		Rendered template.HTML   // the file or README as Markdown renders it
		Size     int64
		Binary   bool
		TooBig   bool
	}{spot: sp, CloneURL: scheme + "://" + r.Host + "/" + p.Repo.FullName() + ".git"}
	p.Data = &v
	if errors.Is(err, store.ErrNotFound) && r.PathValue("rest") == "" {
		return s.render(w, "tree", p) // no default branch yet: an empty repo, with how to clone it
	} else if err != nil {
		return err
	}
	rev := "refs/heads/" + sp.Branch
	typ, size, err := s.Git.Stat(ctx, p.Repo.GitHubID, rev, sp.Path)
	if err != nil {
		return err
	}
	if typ != "tree" && typ != "blob" {
		return store.ErrNotFound
	}
	if last, err := s.Git.Log(ctx, p.Repo.GitHubID, rev, sp.Path, 0, 1); err != nil {
		return err
	} else if len(last) == 1 {
		v.Last = &last[0]
	}
	if v.Commits, err = s.Git.Count(ctx, p.Repo.GitHubID, rev, sp.Path); err != nil {
		return err
	}
	if typ == "blob" {
		v.Size, v.TooBig = size, size > maxFile
		if !v.TooBig {
			if v.Text, err = s.Git.Blob(ctx, p.Repo.GitHubID, rev, sp.Path); err != nil {
				return err
			}
			v.Binary = bytes.IndexByte([]byte(v.Text[:min(len(v.Text), 8000)]), 0) >= 0
			if !v.Binary && v.Text != "" {
				v.Lines = highlight(lexer(sp.Path), strings.TrimSuffix(v.Text, "\n"))
				if v.Markdown = isMarkdown(sp.Path); v.Markdown && r.URL.Query().Get("plain") == "" {
					if v.Rendered, err = markdown(v.Text, p.Repo.FullName(), sp.Branch, sp.Parent); err != nil {
						return err
					}
				}
			}
		}
		return s.render(w, "blob", p)
	}
	if v.Entries, err = s.Git.Tree(ctx, p.Repo.GitHubID, rev, sp.Path); err != nil {
		return err
	}
	for i, e := range v.Entries {
		if e.Type == "blob" && strings.HasPrefix(strings.ToLower(e.Name), "readme") && e.Size <= maxFile {
			v.Readme = &v.Entries[i]
			if v.Text, err = s.Git.Blob(ctx, p.Repo.GitHubID, rev, strings.TrimPrefix(sp.Path+"/"+e.Name, "/")); err != nil {
				return err
			}
			if isMarkdown(e.Name) {
				if v.Rendered, err = markdown(v.Text, p.Repo.FullName(), sp.Branch, sp.Path); err != nil {
					return err
				}
			}
			break
		}
	}
	return s.render(w, "tree", p)
}

// day is the commits of one day, as GitHub groups them.
type day struct {
	Date    string
	Commits []gitstore.Commit
}

// commits lists a branch's commits, or those touching a path in it, a page at a time.
func (s *Server) commits(w http.ResponseWriter, r *http.Request, p *page) error {
	ctx := r.Context()
	sp, err := s.locate(ctx, p, r.PathValue("rest"))
	if err != nil {
		return err
	}
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pg = max(pg, 1)
	list, err := s.Git.Log(ctx, p.Repo.GitHubID, "refs/heads/"+sp.Branch, sp.Path, (pg-1)*commitsPerPage, commitsPerPage+1)
	if err != nil {
		return err
	}
	older := len(list) > commitsPerPage
	list = list[:min(len(list), commitsPerPage)]
	var days []day
	for _, c := range list {
		d := c.When
		if t, err := time.Parse(time.RFC3339, c.When); err == nil {
			d = t.UTC().Format("Jan 2, 2006")
		}
		if len(days) == 0 || days[len(days)-1].Date != d {
			days = append(days, day{Date: d})
		}
		days[len(days)-1].Commits = append(days[len(days)-1].Commits, c)
	}
	v := struct {
		spot
		Days         []day
		Newer, Older int // page numbers, 0 if there's none
	}{spot: sp, Days: days, Newer: pg - 1}
	if older {
		v.Older = pg + 1
	}
	p.Data = v
	return s.render(w, "commits", p)
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// commit shows one commit and what it changed against its first parent.
func (s *Server) commit(w http.ResponseWriter, r *http.Request, p *page) error {
	sha := r.PathValue("sha")
	if !shaRe.MatchString(sha) {
		return store.ErrNotFound
	}
	c, diff, ok, err := s.Git.Show(r.Context(), p.Repo.GitHubID, sha)
	if err != nil {
		return err
	} else if !ok {
		return store.ErrNotFound
	}
	if c.SHA != sha {
		http.Redirect(w, r, fmt.Sprintf("/%s/commit/%s", p.Repo.FullName(), c.SHA), http.StatusSeeOther)
		return nil
	}
	p.Data = struct {
		gitstore.Commit
		Diff diffView
	}{c, parseDiff(diff)}
	return s.render(w, "commit", p)
}
