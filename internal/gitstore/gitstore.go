// Package gitstore keeps one bare repository per mirrored repo and serves it with git http-backend.
//
// Fetches write only GitHub's copy of the refs, under refs/gitcove/github/. The refs served to
// developers (refs/heads, refs/tags, refs/pull) are then moved to match in one atomic update,
// except branches with unreplayed pushes, which stay where developers left them, and restored
// branches. A served ref that would lose commits is first saved under refs/gitcove/history/.
package gitstore

import (
	"bytes"
	"cmp"
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
	"slices"
	"strings"
	"sync"
	"time"
)

const githubNS = "refs/gitcove/github/"

// GitHubHeads is where GitHub's copy of the branches lives.
const GitHubHeads = githubNS + "heads/"

// historyNS holds old tips, as refs/gitcove/history/<stamp>/<ref without refs/>.
const historyNS = "refs/gitcove/history/"

const historyStamp = "20060102T150405.000Z" // milliseconds: two syncs within one second can both save a branch

// RestoredPrefix starts the name of every branch `cove restore` makes. They never follow GitHub.
const RestoredPrefix = "gitcove/restored-"

// served lists the namespaces developers see, each mirrored from githubNS+<namespace without refs/>.
var served = []string{"refs/heads/", "refs/tags/", "refs/pull/"}

type Store struct {
	Root    string        // directory holding <github id>.git
	GitURL  string        // GitHub's Git base URL, e.g. https://github.com
	Git     string        // path to the git binary
	HookBin string        // the cove binary, run as "cove hook pre-receive"
	Keep    time.Duration // how long history refs are kept

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
	s := &Store{Root: root, GitURL: strings.TrimSuffix(gitURL, "/"), Git: git, HookBin: bin, Keep: 90 * 24 * time.Hour}
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
	args := []string{"fetch", "--quiet", "--prune", "--no-tags", "--atomic", "--no-auto-gc", url}
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
// except held and restored branches. Each update names the value it read, so a push landing meanwhile
// makes the whole update fail (and the next sync retry) instead of being overwritten. In the same
// atomic update, a tip that would be lost is saved as a history ref, and expired history is removed.
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
	follows := func(ref string) bool {
		b, ok := strings.CutPrefix(ref, "refs/heads/")
		return !ok || !keep[b] && !strings.HasPrefix(b, RestoredPrefix)
	}
	now := time.Now().UTC()
	stamp := historyNS + now.Format(historyStamp) + "/"
	var cmds bytes.Buffer
	// save records old's tip before ref moves to sha ("" when deleted), unless sha still contains it.
	// refs/pull/N/merge is skipped: GitHub rebuilds it whenever the base moves.
	save := func(ref, old, sha string) {
		if old == "" || strings.HasPrefix(ref, "refs/pull/") && strings.HasSuffix(ref, "/merge") {
			return
		}
		if sha != "" {
			if kept, err := isAncestor(ctx, s, dir, old, sha); err == nil && kept {
				return
			} // on an error, e.g. a tag of a tree, save to be safe
		}
		fmt.Fprintf(&cmds, "create %s%s %s\n", stamp, strings.TrimPrefix(ref, "refs/"), old)
	}
	for ref, sha := range refs {
		if h, ok := strings.CutPrefix(ref, historyNS); ok {
			at, _, _ := strings.Cut(h, "/")
			if t, err := time.Parse(historyStamp, at); err == nil && now.Sub(t) > s.Keep {
				fmt.Fprintf(&cmds, "delete %s %s\n", ref, sha)
			}
			continue
		}
		if gh, ok := strings.CutPrefix(ref, githubNS); ok {
			if want := "refs/" + gh; follows(want) && refs[want] != sha {
				fmt.Fprintf(&cmds, "update %s %s %s\n", want, sha, orZero(refs[want]))
				save(want, refs[want], sha)
			}
			continue
		}
		if _, ok := refs[githubNS+strings.TrimPrefix(ref, "refs/")]; !ok && isServed(ref) && follows(ref) {
			fmt.Fprintf(&cmds, "delete %s %s\n", ref, sha)
			save(ref, sha, "")
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
	killGroup(cmd)
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
			// Hidden refs aren't advertised, and with protocol v0 a client can't ask for what isn't reachable from an advertised one.
			"GIT_CONFIG_KEY_0=uploadpack.hideRefs", "GIT_CONFIG_VALUE_0=refs/gitcove/",
			"GIT_CONFIG_KEY_1=receive.hideRefs", "GIT_CONFIG_VALUE_1=refs/gitcove/",
			"GIT_CONFIG_KEY_2=http.receivepack", "GIT_CONFIG_VALUE_2=true",
			"GIT_CONFIG_KEY_3=core.hooksPath", "GIT_CONFIG_VALUE_3=" + s.hooksDir(),
		}, hookEnv...),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = fmt.Sprintf("/%d.git%s", githubID, path) // http-backend sees only the ID path
		// Protocol v2 serves any object in the repo by ID, hidden or unreachable, such as a
		// force-pushed secret kept in history. Without this header Git falls back to v0.
		r2.Header.Del("Git-Protocol")
		h.ServeHTTP(w, r2)
	})
}

// Saved is a branch tip a sync saved before GitHub moved it backwards, rewrote it or deleted it.
type Saved struct{ At, Branch, Commit string } // At is the history stamp, e.g. 20261115T101500.123Z

// History lists the saved branch tips of githubID's repo, oldest first.
func (s *Store) History(ctx context.Context, githubID int64) ([]Saved, error) {
	refs, err := s.Refs(ctx, s.Dir(githubID))
	if err != nil {
		return nil, err
	}
	var out []Saved
	for ref, sha := range refs {
		at, rest, _ := strings.Cut(strings.TrimPrefix(ref, historyNS), "/")
		if b, ok := strings.CutPrefix(rest, "heads/"); ok && strings.HasPrefix(ref, historyNS) {
			out = append(out, Saved{at, b, sha})
		}
	}
	slices.SortFunc(out, func(a, b Saved) int { return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(a.Branch, b.Branch)) })
	return out, nil
}

// Restore makes the new branch gitcove/restored-<branch>-<at> at the tip of branch saved at at,
// and returns its name and commit. It never moves an existing branch, and syncs leave it alone.
func (s *Store) Restore(ctx context.Context, githubID int64, branch, at string) (name, sha string, err error) {
	dir := s.Dir(githubID)
	out, err := s.run(ctx, dir, nil, "rev-parse", "-q", "--verify", historyNS+at+"/heads/"+branch)
	if exitCode(err) == 1 {
		return "", "", fmt.Errorf("no saved tip of %s at %s", branch, at)
	} else if err != nil {
		return "", "", err
	}
	name, sha = RestoredPrefix+branch+"-"+at, strings.TrimSpace(out)
	_, err = s.run(ctx, dir, nil, "update-ref", "refs/heads/"+name, sha, zeroID)
	return name, sha, err
}
