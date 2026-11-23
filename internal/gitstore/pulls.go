package gitstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// ErrMergeConflict means the two commits can't be merged without a person resolving conflicts.
var ErrMergeConflict = errors.New("merge conflict")

// MergeCommit creates a merge commit of base and head (both commit IDs) authored by who, without
// touching any ref or working tree, so a conflict leaves nothing half-merged. It returns
// ErrMergeConflict if they conflict.
func (s *Store) MergeCommit(ctx context.Context, githubID int64, base, head, who, message string) (string, error) {
	dir := s.Dir(githubID)
	out, err := s.run(ctx, dir, nil, "merge-tree", "--write-tree", "--no-messages", base, head)
	if exitCode(err) == 1 {
		return "", ErrMergeConflict
	} else if err != nil {
		return "", err
	}
	tree, _, _ := strings.Cut(out, "\n")
	email := who + "@gitcove.invalid"
	env := []string{"GIT_AUTHOR_NAME=" + who, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + who, "GIT_COMMITTER_EMAIL=" + email}
	out, err = s.run(ctx, dir, env, "commit-tree", tree, "-p", base, "-p", head, "-m", message)
	return strings.TrimSpace(out), err
}

// MoveBranch points the served branch at commit, only if it is still at old: a push or merge
// landing meanwhile makes it fail instead of being overwritten.
func (s *Store) MoveBranch(ctx context.Context, githubID int64, branch, commit, old string) error {
	_, err := s.run(ctx, s.Dir(githubID), nil, "update-ref", "refs/heads/"+branch, commit, old)
	return err
}

// Diff is what head changes against its merge base with base, as a unified diff: what a PR shows.
func (s *Store) Diff(ctx context.Context, githubID int64, base, head string) (string, error) {
	return s.run(ctx, s.Dir(githubID), nil, "diff", "--no-color", "--no-ext-diff", "--no-textconv", base+"..."+head)
}

// BranchInfo is a served branch and its last commit.
type BranchInfo struct{ Name, Commit, When, Subject string }

// Branches lists the served branches, most recently committed first; none before the repo's first sync.
func (s *Store) Branches(ctx context.Context, githubID int64) ([]BranchInfo, error) {
	if _, err := os.Stat(s.Dir(githubID)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	out, err := s.run(ctx, s.Dir(githubID), nil, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:lstrip=2)%00%(objectname)%00%(committerdate:iso8601-strict)%00%(subject)", "refs/heads/")
	var list []BranchInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Split(line, "\x00"); len(f) == 4 {
			list = append(list, BranchInfo{f[0], f[1], f[2], f[3]})
		}
	}
	return list, err
}
