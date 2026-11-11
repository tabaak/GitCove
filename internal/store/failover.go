package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Mode is the instance mode (design section 6).
type Mode string

const (
	Normal      Mode = "normal"
	Failover    Mode = "failover"
	Reconciling Mode = "reconciling"
	Replaying   Mode = "replaying"
)

// transitions lists, for each mode, the modes it may be entered from.
var transitions = map[Mode][]Mode{
	Failover:    {Normal, Reconciling, Replaying}, // Replaying: GitHub failed again during replay
	Reconciling: {Failover},
	Replaying:   {Reconciling, Normal}, // Normal: replaying items an admin re-queued
	Normal:      {Replaying},           // only once no ref item is pending or sending
}

var ErrWrongMode = errors.New("wrong mode")

func (s *Store) Mode(ctx context.Context) (Mode, error) {
	var m Mode
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = 'mode'").Scan(&m)
	return m, err
}

func mode(tx *sql.Tx) (Mode, error) {
	var m Mode
	err := tx.QueryRow("SELECT value FROM settings WHERE key = 'mode'").Scan(&m)
	return m, err
}

// SetMode moves to mode `to` if the current mode allows it, and audits the change.
// Leaving failover ends the emergency permission to push to protected branches.
func (s *Store) SetMode(ctx context.Context, actor string, to Mode) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		from, err := mode(tx)
		if err != nil {
			return err
		}
		ok := false
		for _, m := range transitions[to] {
			ok = ok || m == from
		}
		if !ok {
			return fmt.Errorf("%w: can't go from %s to %s", ErrWrongMode, from, to)
		}
		if to == Normal {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM outbox WHERE state IN ('pending', 'sending')").Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%w: %d items still to replay", ErrWrongMode, n)
			}
		}
		if _, err := tx.Exec("UPDATE settings SET value = ? WHERE key = 'mode'", to); err != nil {
			return err
		}
		if from == Failover {
			if _, err := tx.Exec("UPDATE settings SET value = '0' WHERE key = 'allow_protected'"); err != nil {
				return err
			}
		}
		return audit(tx, actor, "mode", fmt.Sprintf("%s -> %s", from, to))
	})
}

// AllowProtected turns on or off direct pushes to protected branches for the current failover.
func (s *Store) AllowProtected(ctx context.Context, actor string, on bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Failover {
			return fmt.Errorf("%w: only during failover (mode is %s)", ErrWrongMode, m)
		}
		if _, err := tx.Exec("UPDATE settings SET value = ? WHERE key = 'allow_protected'", boolText(on)); err != nil {
			return err
		}
		return audit(tx, actor, "failover.allow_protected", boolText(on))
	})
}

func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// SetBranches replaces the repo's GitHub branch list with protected flags.
func (s *Store) SetBranches(ctx context.Context, repoID int64, protected map[string]bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM github_branches WHERE repo_id = ?", repoID); err != nil {
			return err
		}
		for b, p := range protected {
			if _, err := tx.Exec("INSERT INTO github_branches (repo_id, branch, protected) VALUES (?, ?, ?)", repoID, b, p); err != nil {
				return err
			}
		}
		_, err := tx.Exec("UPDATE repos SET branches_synced_at = ? WHERE id = ?", now(), repoID)
		return err
	})
}

// MarkBranch adds (on) or removes an admin mark: kind is "protected" or "deploy".
func (s *Store) MarkBranch(ctx context.Context, actor string, repo Repo, branch, kind string, on bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		q := "INSERT OR IGNORE INTO branch_marks (repo_id, branch, kind) VALUES (?, ?, ?)"
		if !on {
			q = "DELETE FROM branch_marks WHERE repo_id = ? AND branch = ? AND kind = ?"
		}
		if _, err := tx.Exec(q, repo.ID, branch, kind); err != nil {
			return err
		}
		return audit(tx, actor, "branch.mark", fmt.Sprintf("%s %s %s=%s", repo.FullName(), branch, kind, boolText(on)))
	})
}

// BranchMark is one admin mark, for listing.
type BranchMark struct{ Repo, Branch, Kind string }

func (s *Store) BranchMarks(ctx context.Context) ([]BranchMark, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.owner || '/' || r.name, m.branch, m.kind FROM branch_marks m
		JOIN repos r ON r.id = m.repo_id ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BranchMark
	for rows.Next() {
		var m BranchMark
		if err := rows.Scan(&m.Repo, &m.Branch, &m.Kind); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetPROnly turns the PR-only rule for default and deploy branches on or off for a repo.
func (s *Store) SetPROnly(ctx context.Context, actor string, repo Repo, on bool) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE repos SET pr_only = ? WHERE id = ?", on, repo.ID); err != nil {
			return err
		}
		return audit(tx, actor, "repo.pr_only", fmt.Sprintf("%s %s", repo.FullName(), boolText(on)))
	})
}

// Policy is what push and replay decisions need to know about a repo's branches.
type Policy struct {
	Mode           Mode
	AllowProtected bool
	DefaultBranch  string
	PROnlyOn       bool
	BranchesSynced bool
	GitHub         map[string]bool // branch -> GitHub's protected flag
	Protected      map[string]bool // admin-listed
	Deploy         map[string]bool // admin-listed
}

// IsProtected says whether pushes to branch are refused. onGitHub says whether GitHub has the
// branch. A new branch is left for GitHub to judge at replay; an existing branch whose
// protection GitCove doesn't know counts as protected.
func (p Policy) IsProtected(branch string, onGitHub bool) bool {
	if p.Protected[branch] {
		return true
	}
	if !onGitHub {
		return false
	}
	prot, known := p.GitHub[branch]
	return !p.BranchesSynced || !known || prot
}

// PROnly says whether work for branch may reach GitHub only through a PR (design 8.7).
func (p Policy) PROnly(branch string, onGitHub bool) bool {
	return p.PROnlyOn && (branch == p.DefaultBranch || p.Deploy[branch]) || p.IsProtected(branch, onGitHub)
}

func (s *Store) Policy(ctx context.Context, repoID int64) (Policy, error) {
	p := Policy{GitHub: map[string]bool{}, Protected: map[string]bool{}, Deploy: map[string]bool{}}
	var synced sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT value FROM settings WHERE key = 'mode'),
		(SELECT value FROM settings WHERE key = 'allow_protected') = '1', default_branch, pr_only, branches_synced_at
		FROM repos WHERE id = ?`, repoID).Scan(&p.Mode, &p.AllowProtected, &p.DefaultBranch, &p.PROnlyOn, &synced)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	} else if err != nil {
		return p, err
	}
	p.BranchesSynced = synced.Valid
	rows, err := s.db.QueryContext(ctx, `SELECT branch, protected, 'github' FROM github_branches WHERE repo_id = ?
		UNION ALL SELECT branch, 1, kind FROM branch_marks WHERE repo_id = ?`, repoID, repoID)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var b, kind string
		var v bool
		if err := rows.Scan(&b, &v, &kind); err != nil {
			return p, err
		}
		switch kind {
		case "github":
			p.GitHub[b] = v
		case "protected":
			p.Protected[b] = true
		case "deploy":
			p.Deploy[b] = true
		}
	}
	return p, rows.Err()
}

// RefUpdate is one accepted branch update from a push.
type RefUpdate struct{ Branch, Old, New string }

// RefItem is a queued push, one per updated branch.
type RefItem struct {
	ID       int64
	OpID     string
	RepoID   int64
	Repo     string // owner/name
	Branch   string
	Old, New string
	Pusher   string
	State    string
	Target   string
	Result   string
	Created  string
}

type refPayload struct {
	Branch string `json:"branch"`
	New    string `json:"new"`
	Pusher string `json:"pusher"`
}

// RecordPush queues a push's branch updates, one transaction for all of them. It fails with
// ErrWrongMode unless the instance is in failover, checked inside the same transaction.
func (s *Store) RecordPush(ctx context.Context, repoID int64, pusher string, updates []RefUpdate) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Failover {
			return fmt.Errorf("%w: mode is %s", ErrWrongMode, m)
		}
		for _, u := range updates {
			payload, _ := json.Marshal(refPayload{u.Branch, u.New, pusher})
			if _, err := tx.Exec("INSERT INTO outbox (op_id, repo_id, kind, base_version, payload) VALUES (?, ?, 'ref', ?, ?)",
				opID(), repoID, u.Old, payload); err != nil {
				return err
			}
		}
		return nil
	})
}

// opID is an outbox item's operation ID, the marker replay puts in what it posts (design 8.4).
func opID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// HeldBranches lists the repo's branches with unreplayed or unresolved pushes. Their served refs
// stay where developers left them instead of following GitHub.
func (s *Store) HeldBranches(ctx context.Context, repoID int64) (map[string]bool, error) {
	items, err := s.RefItems(ctx, repoID, "pending", "sending", "conflict", "uncertain")
	held := map[string]bool{}
	for _, it := range items {
		held[it.Branch] = true
	}
	return held, err
}

// RefItems lists ref items in the given states, oldest first. repoID 0 means every repo.
func (s *Store) RefItems(ctx context.Context, repoID int64, states ...string) ([]RefItem, error) {
	args := []any{repoID, repoID}
	for _, st := range states {
		args = append(args, st)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.op_id, o.repo_id, r.owner || '/' || r.name, coalesce(o.base_version, ''),
		o.payload, o.state, coalesce(o.target, ''), coalesce(o.result, ''), o.created_at
		FROM outbox o JOIN repos r ON r.id = o.repo_id
		WHERE o.kind = 'ref' AND (? = 0 OR o.repo_id = ?) AND o.state IN (?`+strings.Repeat(", ?", len(states)-1)+`)
		ORDER BY o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefItem
	for rows.Next() {
		var it RefItem
		var payload []byte
		if err := rows.Scan(&it.ID, &it.OpID, &it.RepoID, &it.Repo, &it.Old, &payload, &it.State, &it.Target, &it.Result, &it.Created); err != nil {
			return nil, err
		}
		var p refPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("outbox %d: %w", it.ID, err)
		}
		it.Branch, it.New, it.Pusher = p.Branch, p.New, p.Pusher
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkSending moves pending or sending items to sending with the given target branch, only
// while replaying. It's written before any request, so a restart knows a push may have landed.
func (s *Store) MarkSending(ctx context.Context, ids []int64, target string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Replaying {
			return fmt.Errorf("%w: mode is %s", ErrWrongMode, m)
		}
		return updateItems(tx, ids, "state = 'sending', target = ?", target)
	})
}

// Finish ends pending or sending items as done, conflict or uncertain. A conflict or uncertain
// item also gets a conflicts row with both versions (e.g. the commit on GitCove and GitHub's tip).
func (s *Store) Finish(ctx context.Context, ids []int64, state, result, local, github string) error {
	if state != "done" && state != "conflict" && state != "uncertain" {
		return fmt.Errorf("finish: bad state %q", state)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := updateItems(tx, ids, "state = ?, result = ?", state, result); err != nil {
			return err
		}
		if state != "done" {
			for _, id := range ids {
				if _, err := tx.Exec("INSERT INTO conflicts (outbox_id, reason, local_version, github_version) VALUES (?, ?, ?, ?)",
					id, result, local, github); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func updateItems(tx *sql.Tx, ids []int64, set string, args ...any) error {
	for _, id := range ids {
		if _, err := tx.Exec("UPDATE outbox SET "+set+", updated_at = ? WHERE id = ? AND state IN ('pending', 'sending')",
			append(args, now(), id)...); err != nil {
			return err
		}
	}
	return nil
}

// Resolve is an admin's decision on a conflict or uncertain item: skip it for good, or retry it.
func (s *Store) Resolve(ctx context.Context, actor string, id int64, retry bool) error {
	to, action := "skipped", "replay.skip"
	if retry {
		to, action = "pending", "replay.retry"
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE outbox SET state = ?, updated_at = ? WHERE id = ? AND state IN ('conflict', 'uncertain')", to, now(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("item %d: %w, or not in conflict or uncertain", id, ErrNotFound)
		}
		if _, err := tx.Exec("UPDATE conflicts SET resolved_at = ? WHERE outbox_id = ? AND resolved_at IS NULL", now(), id); err != nil {
			return err
		}
		return audit(tx, actor, action, fmt.Sprintf("id=%d", id))
	})
}
