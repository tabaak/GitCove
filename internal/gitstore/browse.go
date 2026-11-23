package gitstore

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// Read-only views for the web interface's Code tab. Callers pass served branches as revs; Show
// checks a commit itself, so nothing reachable only from refs/gitcove/ is ever shown.

// literal stops paths given to log and rev-list from being read as pathspec magic, such as ":(glob)".
var literal = []string{"GIT_LITERAL_PATHSPECS=1"}

// Entry is a file or directory in a tree.
type Entry struct {
	Name, Type string // Type is "tree", "blob", or "commit" for a submodule
	Size       int64  // of a blob
}

// Stat returns the type ("tree", "blob", ...) and size of path ("" for the top) in rev, or "" if there's nothing there.
func (s *Store) Stat(ctx context.Context, githubID int64, rev, path string) (string, int64, error) {
	out, err := s.runIn(ctx, s.Dir(githubID), nil, strings.NewReader(rev+":"+path+"\n"), "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	out = strings.TrimSpace(out)
	if err != nil || strings.HasSuffix(out, " missing") || strings.HasSuffix(out, " ambiguous") {
		return "", 0, err
	}
	typ, size, _ := strings.Cut(out, " ")
	n, _ := strconv.ParseInt(size, 10, 64)
	return typ, n, nil
}

// Tree lists the directory at path ("" for the top) in rev, directories first.
func (s *Store) Tree(ctx context.Context, githubID int64, rev, path string) ([]Entry, error) {
	out, err := s.run(ctx, s.Dir(githubID), nil, "ls-tree", "-z", "-l", rev+":"+path)
	var list []Entry
	for _, rec := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); ok && len(f) == 4 {
			size, _ := strconv.ParseInt(f[3], 10, 64)
			list = append(list, Entry{name, f[1], size})
		}
	}
	slices.SortStableFunc(list, func(a, b Entry) int { return boolInt(b.Type == "tree") - boolInt(a.Type == "tree") })
	return list, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Blob returns the content of the file at path in rev. Check its size with Stat first.
func (s *Store) Blob(ctx context.Context, githubID int64, rev, path string) (string, error) {
	return s.run(ctx, s.Dir(githubID), nil, "cat-file", "blob", rev+":"+path)
}

// Commit is a commit as the web interface shows it.
type Commit struct {
	SHA, Author, Email, When, Subject, Body string
	Parents                                 []string
}

const commitFormat = "--format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%s%x00%b%x1e"

func parseCommits(out string) []Commit {
	var list []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		if f := strings.Split(strings.TrimLeft(rec, "\n"), "\x00"); len(f) == 7 {
			list = append(list, Commit{f[0], f[2], f[3], f[4], f[5], strings.TrimSpace(f[6]), strings.Fields(f[1])})
		}
	}
	return list
}

// Log lists up to n commits of rev that touch path ("" for all), newest first, after skipping skip.
func (s *Store) Log(ctx context.Context, githubID int64, rev, path string, skip, n int) ([]Commit, error) {
	args := []string{"log", commitFormat, "--skip=" + strconv.Itoa(skip), "-n", strconv.Itoa(n), rev, "--"}
	if path != "" {
		args = append(args, path)
	}
	out, err := s.run(ctx, s.Dir(githubID), literal, args...)
	return parseCommits(out), err
}

// Count is how many commits of rev touch path ("" for all).
func (s *Store) Count(ctx context.Context, githubID int64, rev, path string) (int, error) {
	args := []string{"rev-list", "--count", rev, "--"}
	if path != "" {
		args = append(args, path)
	}
	out, err := s.run(ctx, s.Dir(githubID), literal, args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// Show returns commit sha and its changes against its first parent as a unified diff. ok is false
// unless a served ref contains it: hidden refs keep force-pushed commits, which must stay unseen.
func (s *Store) Show(ctx context.Context, githubID int64, sha string) (c Commit, diff string, ok bool, err error) {
	dir := s.Dir(githubID)
	full, err := s.run(ctx, dir, nil, "rev-parse", "-q", "--verify", sha+"^{commit}")
	if exitCode(err) == 1 {
		return c, "", false, nil
	} else if err != nil {
		return c, "", false, err
	}
	full = strings.TrimSpace(full)
	refs, err := s.run(ctx, dir, nil, append([]string{"for-each-ref", "--count=1", "--format=%(refname)", "--contains", full}, served...)...)
	if err != nil || strings.TrimSpace(refs) == "" {
		return c, "", false, err
	}
	out, err := s.run(ctx, dir, nil, "show", commitFormat, "--no-color", "--no-ext-diff", "--no-textconv", "--diff-merges=first-parent", full)
	if err != nil {
		return c, "", false, err
	}
	head, diff, _ := strings.Cut(out, "\x1e")
	list := parseCommits(head + "\x1e")
	if len(list) != 1 {
		return c, "", false, nil
	}
	return list[0], diff, true, nil
}
