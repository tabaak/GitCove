package gitstore

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// GitError is a failed git command.
type GitError struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *GitError) Error() string { return fmt.Sprintf("git %s: %v: %s", e.Args[0], e.Err, e.Stderr) }
func (e *GitError) Unwrap() error { return e.Err }

// exitCode is the git process's exit code, or -1 if it didn't run or was killed.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// RejectedError is a push GitHub answered and refused, as opposed to one that failed in transit.
type RejectedError struct {
	NonFastForward bool   // GitHub's branch has commits the pushed one lacks
	Reason         string // git's summary plus GitHub's message
}

func (e *RejectedError) Error() string { return "push rejected: " + e.Reason }

// Push sends sha to branch on GitHub. It never forces, so GitHub accepts only a fast-forward or a
// new branch (R1). A refusal comes back as *RejectedError; anything else may or may not have landed.
func (s *Store) Push(ctx context.Context, githubID int64, owner, name, token, sha, branch string) error {
	if err := checkName(owner, name); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%s/%s.git", s.GitURL, owner, name)
	out, err := s.run(ctx, s.Dir(githubID), authEnv(token), "push", "--porcelain", "--quiet", url, sha+":refs/heads/"+branch)
	if err == nil {
		return nil
	}
	// --porcelain prints "!<tab>from:to<tab>[rejected] (reason)" for each refused ref.
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, "\t"); len(f) == 3 && f[0] == "!" {
			if strings.Contains(f[2], "failed to update ref") || strings.Contains(f[2], "failed to lock") {
				return err // GitHub's side was busy (e.g. an abandoned earlier push still holds the lock): retry
			}
			var ge *GitError
			errors.As(err, &ge)
			nonFF := strings.Contains(f[2], "non-fast-forward") || strings.Contains(f[2], "fetch first")
			return &RejectedError{NonFastForward: nonFF, Reason: strings.TrimSpace(f[2] + "\n" + ge.Stderr)}
		}
	}
	return err
}

// Ref returns the object ref points to in githubID's repo, or "" if it doesn't exist.
func (s *Store) Ref(ctx context.Context, githubID int64, ref string) (string, error) {
	out, err := s.run(ctx, s.Dir(githubID), nil, "rev-parse", "-q", "--verify", ref+"^{commit}")
	if exitCode(err) == 1 {
		return "", nil
	}
	return strings.TrimSpace(out), err
}

// IsAncestor reports whether commit a is b or an ancestor of b.
func (s *Store) IsAncestor(ctx context.Context, githubID int64, a, b string) (bool, error) {
	return isAncestor(ctx, s, s.Dir(githubID), a, b)
}

func isAncestor(ctx context.Context, s *Store, dir, a, b string) (bool, error) {
	_, err := s.run(ctx, dir, nil, "merge-base", "--is-ancestor", a, b)
	if exitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

// GitHubBranchesContaining lists GitHub's branches (as last fetched) whose tip contains sha.
func (s *Store) GitHubBranchesContaining(ctx context.Context, githubID int64, sha string) ([]string, error) {
	out, err := s.run(ctx, s.Dir(githubID), nil, "for-each-ref", "--contains", sha, "--format=%(refname)", GitHubHeads)
	var names []string
	for _, ref := range strings.Fields(out) {
		names = append(names, strings.TrimPrefix(ref, GitHubHeads))
	}
	return names, err
}

// WorkflowsMatchGitHub reports whether sha's .github/workflows tree equals that of some branch tip
// on GitHub (as last fetched). Without the workflows permission GitHub accepts a push only then (spike W).
func (s *Store) WorkflowsMatchGitHub(ctx context.Context, githubID int64, sha string) (bool, error) {
	dir := s.Dir(githubID)
	tips, err := s.run(ctx, dir, nil, "for-each-ref", "--format=%(objectname)", GitHubHeads)
	if err != nil {
		return false, err
	}
	// One cat-file for all of them: "<oid> tree <size>" per present tree, "<input> missing" otherwise.
	in := sha + ":.github/workflows\n"
	for _, tip := range strings.Fields(tips) {
		in += tip + ":.github/workflows\n"
	}
	out, err := s.runIn(ctx, dir, nil, strings.NewReader(in), "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return false, err
	}
	trees := strings.Split(strings.TrimSpace(out), "\n")
	for i, t := range trees {
		if strings.HasSuffix(t, " missing") { // no workflows directory: all such tips match each other
			trees[i] = "missing"
		}
	}
	for _, t := range trees[1:] {
		if t == trees[0] {
			return true, nil
		}
	}
	return false, nil
}
