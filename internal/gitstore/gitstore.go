// Package gitstore keeps one bare repository per mirrored repo and serves it with git http-backend.
//
// Fetches write only GitHub's copy of the refs, under refs/gitcove/github/. The refs served to
// developers (refs/heads, refs/tags, refs/pull) are then moved to match in one atomic update,
// except branches with unreplayed pushes, which stay where developers left them.
package gitstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const githubNS = "refs/gitcove/github/"

// GitHubHeads is where GitHub's copy of the branches lives.
const GitHubHeads = githubNS + "heads/"

// served lists the namespaces developers see, each mirrored from githubNS+<namespace without refs/>.
var served = []string{"refs/heads/", "refs/tags/", "refs/pull/"}

type Store struct {
	Root    string // directory holding <github id>.git
	GitURL  string // GitHub's Git base URL, e.g. https://github.com
	Git     string // path to the git binary
	HookBin string // the cove binary, run as "cove hook pre-receive"

	locks sync.Map // github id -> *sync.Mutex, so a sync and a replay don't fetch one repo at once
}

// New finds git on PATH, creates root, and installs the pre-receive hook that every served repo uses.
func New(root, gitURL string) (*Store, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	// Absolute, because http-backend runs from the git binary's directory, not ours.
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	s := &Store{Root: root, GitURL: strings.TrimSuffix(gitURL, "/"), Git: git, HookBin: bin}
	if err := os.MkdirAll(s.hooksDir(), 0o700); err != nil {
		return nil, err
	}
	// The binary's path comes from the environment http-backend passes on, so the script never changes.
	hook := "#!/bin/sh\nexec \"$COVE_HOOK_BIN\" hook pre-receive\n"
	return s, os.WriteFile(filepath.Join(s.hooksDir(), "pre-receive"), []byte(hook), 0o700)
}

// hooksDir is outside every repo, so nothing a client pushes can change the hooks.
func (s *Store) hooksDir() string { return filepath.Join(s.Root, "_hooks") }

func (s *Store) lock(githubID int64) func() {
	m, _ := s.locks.LoadOrStore(githubID, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return m.(*sync.Mutex).Unlock
}

// Dir is the bare repository for a GitHub repo ID. IDs, not names, so renames keep the directory.
func (s *Store) Dir(githubID int64) string {
	return filepath.Join(s.Root, fmt.Sprintf("%d.git", githubID))
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func checkName(owner, name string) error {
	if !safeName.MatchString(owner) || !safeName.MatchString(name) || strings.HasPrefix(owner, ".") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("unsafe repo name %q/%q", owner, name)
	}
	return nil
}

// authEnv hands git the token through the environment, never argv, so it doesn't show in process listings.
func authEnv(token string) []string {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic " + auth}
}

// Fetch brings GitHub's refs into refs/gitcove/github/ and then moves the served refs to match,
// leaving the branches in held alone. A failed or cut fetch changes no refs, so the next call simply retries.
// held is called after the fetch, so it sees pushes recorded while the fetch ran.
func (s *Store) Fetch(ctx context.Context, githubID int64, owner, name, defaultBranch, token string, held func() (map[string]bool, error)) error {
	if err := checkName(owner, name); err != nil {
		return err
	}
	defer s.lock(githubID)()
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
	url := fmt.Sprintf("%s/%s/%s.git", s.GitURL, owner, name)
	args := []string{"fetch", "--quiet", "--prune", "--no-tags", "--atomic", url}
	for _, ns := range served {
		args = append(args, "+"+ns+"*:"+githubNS+strings.TrimPrefix(ns, "refs/")+"*")
	}
	if _, err := s.run(ctx, dir, authEnv(token), args...); err != nil {
		return err
	}
	if err := s.updateServed(ctx, dir, held); err != nil {
		return err
	}
	if defaultBranch != "" {
		_, err := s.run(ctx, dir, nil, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch)
		return err
	}
	return nil
}

// updateServed points every served ref at GitHub's copy and deletes served refs GitHub no longer has,
// except held branches. Each update names the value it read, so a push landing meanwhile makes the
// whole update fail (and the next sync retry) instead of being overwritten.
func (s *Store) updateServed(ctx context.Context, dir string, held func() (map[string]bool, error)) error {
	refs, err := s.Refs(ctx, dir)
	if err != nil {
		return err
	}
	// Read after the refs: a push records its outbox row before moving its ref, so either the row
	// is visible here or the ref has moved and the compare-and-swap below fails.
	keep, err := held()
	if err != nil {
		return err
	}
	var cmds bytes.Buffer
	for ref, sha := range refs {
		if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && keep[b] {
			continue
		}
		if gh, ok := strings.CutPrefix(ref, githubNS); ok {
			if b, ok := strings.CutPrefix(gh, "heads/"); ok && keep[b] {
				continue
			}
			if want := "refs/" + gh; refs[want] != sha {
				fmt.Fprintf(&cmds, "update %s %s %s\n", want, sha, orZero(refs[want]))
			}
			continue
		}
		if _, ok := refs[githubNS+strings.TrimPrefix(ref, "refs/")]; !ok && isServed(ref) {
			fmt.Fprintf(&cmds, "delete %s %s\n", ref, sha)
		}
	}
	if cmds.Len() == 0 {
		return nil
	}
	// update-ref --stdin applies all lines atomically.
	_, err = s.runIn(ctx, dir, nil, &cmds, "update-ref", "--stdin")
	return err
}

// orZero gives update-ref's "must not exist" value for a ref that's absent.
func orZero(sha string) string {
	if sha == "" {
		return zeroID
	}
	return sha
}

// zeroID is Git's all-zero object ID (SHA-1 repos), meaning "no ref".
const zeroID = "0000000000000000000000000000000000000000"

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

func (s *Store) runIn(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Git, args...)
	cmd.Dir = dir
	// Ignore the user's and system Git config: `cove client on` there would send GitCove's own
	// fetches and pushes back to itself, and a credential helper there could pop up a login.
	cmd.Env = append(os.Environ(), append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")...)
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
		return stdout.String(), &GitError{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), nil
}

// Handler serves the repo for githubID over Git smart HTTP with GitCove's own refs hidden.
// path is the part of the URL after the repo, e.g. /info/refs. Pushes go through the
// pre-receive hook, which gets hookEnv (database, repo and pusher) and decides what to accept.
func (s *Store) Handler(githubID int64, path string, hookEnv ...string) http.Handler {
	h := &cgi.Handler{
		Path: s.Git,
		Args: []string{"http-backend"},
		Env: append([]string{
			"GIT_PROJECT_ROOT=" + s.Root,
			"GIT_HTTP_EXPORT_ALL=1",
			"COVE_HOOK_BIN=" + s.HookBin,
			"GIT_CONFIG_COUNT=4",
			"GIT_CONFIG_KEY_0=uploadpack.hideRefs", "GIT_CONFIG_VALUE_0=refs/gitcove/",
			"GIT_CONFIG_KEY_1=receive.hideRefs", "GIT_CONFIG_VALUE_1=refs/gitcove/",
			"GIT_CONFIG_KEY_2=http.receivepack", "GIT_CONFIG_VALUE_2=true",
			"GIT_CONFIG_KEY_3=core.hooksPath", "GIT_CONFIG_VALUE_3=" + s.hooksDir(),
		}, hookEnv...),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = fmt.Sprintf("/%d.git%s", githubID, path) // http-backend sees only the ID path
		h.ServeHTTP(w, r2)
	})
}
