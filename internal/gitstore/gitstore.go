// Package gitstore keeps one bare repository per mirrored repo and serves it with git http-backend.
//
// Fetches write only GitHub's copy of the refs, under refs/gitcove/github/. The refs served to
// developers (refs/heads, refs/tags, refs/pull) are then moved to match in one atomic update.
package gitstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const githubNS = "refs/gitcove/github/"

// served lists the namespaces developers see, each mirrored from githubNS+<namespace without refs/>.
var served = []string{"refs/heads/", "refs/tags/", "refs/pull/"}

type Store struct {
	Root   string // directory holding <github id>.git
	GitURL string // GitHub's Git base URL, e.g. https://github.com
	Git    string // path to the git binary
}

// New finds git on PATH and creates root.
func New(root, gitURL string) (*Store, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	// Absolute, because http-backend runs from the git binary's directory, not ours.
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	return &Store{Root: root, GitURL: strings.TrimSuffix(gitURL, "/"), Git: git}, os.MkdirAll(root, 0o700)
}

// Dir is the bare repository for a GitHub repo ID. IDs, not names, so renames keep the directory.
func (s *Store) Dir(githubID int64) string {
	return filepath.Join(s.Root, fmt.Sprintf("%d.git", githubID))
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Fetch brings GitHub's refs into refs/gitcove/github/ and then moves the served refs to match.
// A failed or cut fetch changes no refs, so the next call simply retries.
func (s *Store) Fetch(ctx context.Context, githubID int64, owner, name, defaultBranch, token string) error {
	if !safeName.MatchString(owner) || !safeName.MatchString(name) || strings.HasPrefix(owner, ".") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("unsafe repo name %q/%q", owner, name)
	}
	dir := s.Dir(githubID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Init in a temp dir and rename, so a crash never leaves a half-made repo at dir.
		tmp := dir + ".tmp"
		os.RemoveAll(tmp)
		if _, err := s.run(ctx, "", nil, "init", "--bare", "-q", tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return err
		}
	}
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	// The token goes in through the environment, never argv, so it doesn't show in process listings.
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + auth}
	url := fmt.Sprintf("%s/%s/%s.git", s.GitURL, owner, name)
	args := []string{"fetch", "--quiet", "--prune", "--no-tags", "--atomic", url}
	for _, ns := range served {
		args = append(args, "+"+ns+"*:"+githubNS+strings.TrimPrefix(ns, "refs/")+"*")
	}
	if _, err := s.run(ctx, dir, env, args...); err != nil {
		return err
	}
	if err := s.updateServed(ctx, dir); err != nil {
		return err
	}
	if defaultBranch != "" {
		_, err := s.run(ctx, dir, nil, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
		return err
	}
	return nil
}

// updateServed points every served ref at GitHub's copy and deletes served refs GitHub no longer has.
// shortcut: every branch follows GitHub; milestone 2 keeps branches with unreplayed work where developers left them.
func (s *Store) updateServed(ctx context.Context, dir string) error {
	refs, err := s.Refs(ctx, dir)
	if err != nil {
		return err
	}
	var cmds bytes.Buffer
	for ref, sha := range refs {
		if gh, ok := strings.CutPrefix(ref, githubNS); ok {
			if want := "refs/" + gh; refs[want] != sha {
				fmt.Fprintf(&cmds, "update %s %s\n", want, sha)
			}
			continue
		}
		if _, ok := refs[githubNS+strings.TrimPrefix(ref, "refs/")]; !ok && isServed(ref) {
			fmt.Fprintf(&cmds, "delete %s\n", ref)
		}
	}
	if cmds.Len() == 0 {
		return nil
	}
	// update-ref --stdin applies all lines atomically.
	_, err = s.runIn(ctx, dir, nil, &cmds, "update-ref", "--stdin")
	return err
}

func isServed(ref string) bool {
	for _, ns := range served {
		if strings.HasPrefix(ref, ns) {
			return true
		}
	}
	return false
}

// Refs returns every ref in the repo at dir with the object it points to.
func (s *Store) Refs(ctx context.Context, dir string) (map[string]string, error) {
	out, err := s.run(ctx, dir, nil, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if ref, sha, ok := strings.Cut(line, " "); ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}

func (s *Store) run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return s.runIn(ctx, dir, env, nil, args...)
}

func (s *Store) runIn(ctx context.Context, dir string, env []string, stdin *bytes.Buffer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(env, "GIT_TERMINAL_PROMPT=0")...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	// Kill git's helpers too (git-remote-https holds the network connection), not only git itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Handler serves the repo for githubID over Git smart HTTP, read-only, with GitCove's own refs hidden.
// path is the part of the URL after the repo, e.g. /info/refs.
func (s *Store) Handler(githubID int64, path string) http.Handler {
	h := &cgi.Handler{
		Path: s.Git,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.Root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=uploadpack.hideRefs", "GIT_CONFIG_VALUE_0=refs/gitcove/",
			"GIT_CONFIG_KEY_1=http.receivepack", "GIT_CONFIG_VALUE_1=false",
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = fmt.Sprintf("/%d.git%s", githubID, path) // http-backend sees only the ID path
		h.ServeHTTP(w, r2)
	})
}
