package gitstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tabaak/gitcove/internal/store"
)

// RunHook is "cove hook pre-receive". git-receive-pack runs it inside the bare repo before
// updating any ref; http-backend passes on COVE_DB, COVE_REPO_ID and COVE_PUSHER from Handler.
// Its output reaches the pushing developer as "remote:" lines. It returns the exit code.
func RunHook(args []string) int {
	if len(args) != 1 || args[0] != "pre-receive" {
		fmt.Fprintln(os.Stderr, "usage: cove hook pre-receive")
		return 2
	}
	msg, err := func() (string, error) {
		repoID, err := strconv.ParseInt(os.Getenv("COVE_REPO_ID"), 10, 64)
		if err != nil {
			return "", err
		}
		st, err := store.Open(os.Getenv("COVE_DB"))
		if err != nil {
			return "", err
		}
		defer st.Close()
		git, err := exec.LookPath("git")
		if err != nil {
			return "", err
		}
		return (&Store{Git: git}).preReceive(context.Background(), st, ".", repoID, os.Getenv("COVE_PUSHER"), os.Stdin)
	}()
	var r rejection
	switch {
	case err == nil:
		fmt.Fprintf(os.Stderr, "GitCove: %s\n", msg)
		return 0
	case errors.As(err, &r):
		fmt.Fprintf(os.Stderr, "GitCove: %s\n", r)
	default: // details stay on the server; the developer just retries
		fmt.Fprintln(os.Stderr, "GitCove: internal error, push refused. Try again, and tell the admin if it persists.")
	}
	return 1
}

// rejection is a refusal meant for the developer, as opposed to an internal error.
type rejection string

func (r rejection) Error() string { return string(r) }

func reject(format string, args ...any) error { return rejection(fmt.Sprintf(format, args...)) }

func isZero(sha string) bool { return strings.Trim(sha, "0") == "" }

// preReceive accepts a push only in failover or degraded mode, only to branches, never deleting or
// rewriting one, and never to a protected branch unless the admin allowed it for this failover.
// Accepted updates are queued for replay before git moves any ref, so a crash can't leave a push
// unrecorded; if the push then fails, replay finds nothing new on the branch and pushes nothing.
// It returns the line the developer sees for an accepted push.
func (s *Store) preReceive(ctx context.Context, st *store.Store, dir string, repoID int64, pusher string, in io.Reader) (string, error) {
	p, err := st.Policy(ctx, repoID)
	if err != nil {
		return "", err
	}
	notFailover := reject("pushes are accepted only during failover or degraded mode. Push to GitHub.")
	if !p.Mode.TakesWrites() {
		return "", notFailover
	}
	var ups []store.RefUpdate
	asPR := false
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			return "", fmt.Errorf("bad pre-receive line %q", sc.Text())
		}
		old, sha, ref := f[0], f[1], f[2]
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok {
			return "", reject("%s: only branches can be pushed during failover.", ref)
		}
		if isZero(sha) {
			return "", reject("%s: branches can't be deleted during failover.", branch)
		}
		if !isZero(old) {
			ff, err := isAncestor(ctx, s, dir, old, sha)
			if err != nil {
				return "", err
			}
			if !ff {
				return "", reject("%s: force-push refused; GitCove never rewrites history. Pull, merge and push again.", branch)
			}
		}
		_, err := s.run(ctx, dir, nil, "rev-parse", "-q", "--verify", GitHubHeads+branch)
		if code := exitCode(err); err != nil && code != 1 {
			return "", err
		} else if p.IsProtected(branch, err == nil) && !p.AllowProtected {
			return "", reject("%s is protected, so it changes only through a reviewed merge. Push your work to another branch:\n"+
				"  git push origin HEAD:refs/heads/<new-branch>\n"+
				"It reaches GitHub after the outage, and you can open a PR there.", branch)
		}
		asPR = asPR || err == nil && p.PROnly(branch, true) // as replay decides it
		ups = append(ups, store.RefUpdate{Branch: branch, Old: old, New: sha})
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if err := st.RecordPush(ctx, repoID, pusher, ups); errors.Is(err, store.ErrWrongMode) {
		return "", notFailover // failover ended while this push was arriving
	} else if err != nil {
		return "", err
	}
	return acceptedMsg(p.Mode, asPR), nil
}

// acceptedMsg tells the developer when an accepted push reaches GitHub, and how.
func acceptedMsg(mode store.Mode, asPR bool) string {
	msg := "GitHub is down. Your push is saved on GitCove and will be sent to GitHub when it's back"
	switch mode {
	case store.Degraded:
		msg = "GitHub is slow or flaky. Your push is saved on GitCove and will be sent to GitHub shortly"
	case store.Paused:
		msg = "GitHub is slow or flaky. Your push is saved on GitCove and will be sent to GitHub once the admin resumes replay"
	}
	if asPR {
		msg += ", as a PR"
	}
	return msg + "."
}
