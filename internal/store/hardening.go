package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ScanDue in a repo's full scan cursor makes its next sync a full scan.
const ScanDue = "due"

// FullScanCursor names the cursor holding when repoID's last full scan finished.
func FullScanCursor(repoID int64) string { return fmt.Sprintf("repo:%d:full_scan", repoID) }

// QueueWebhook records delivery and marks the repo with GitHub ID githubRepoID for a sync, in one
// transaction, so a delivery counts as seen only once its hint is durable. deleted also makes that
// sync a full scan, the only kind that sees deletions. It returns false for a delivery already seen.
func (s *Store) QueueWebhook(ctx context.Context, delivery string, githubRepoID int64, deleted bool) (bool, error) {
	queued := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// GitHub redelivers for up to 3 days, so a week of IDs is plenty.
		if _, err := tx.Exec("DELETE FROM webhook_deliveries WHERE received_at < ?", time.Now().UTC().Add(-7*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
			return err
		}
		res, err := tx.Exec("INSERT INTO webhook_deliveries (id) VALUES (?) ON CONFLICT DO NOTHING", delivery)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		queued = true
		var id int64
		err = tx.QueryRow("UPDATE repos SET sync_wanted = 1 WHERE github_id = ? RETURNING id", githubRepoID).Scan(&id)
		if err == sql.ErrNoRows {
			return nil // a repo not synced yet: the next full cycle picks it up
		} else if err != nil || !deleted {
			return err
		}
		_, err = tx.Exec(`INSERT INTO sync_cursors (name, cursor, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`, FullScanCursor(id), ScanDue, now())
		return err
	})
	return queued, err
}

// WantedRepos lists the repos a webhook asked to sync.
func (s *Store) WantedRepos(ctx context.Context) ([]Repo, error) {
	return s.repos(ctx, "WHERE sync_wanted = 1")
}

// ClearWanted is called as a repo's sync starts, so a webhook arriving during the sync asks for another.
func (s *Store) ClearWanted(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE repos SET sync_wanted = 0 WHERE id = ?", repoID)
	return err
}

// Tombstone kinds, each with the table it buries rows from and the version it keeps.
var buried = map[string]struct{ table, id, item string }{
	"issue": {"issues", "number", `json_object('number', number, 'is_pr', is_pr, 'title', title, 'body', body, 'state', state,
		'author', author, 'labels', json(labels), 'created_at', created_at, 'updated_at', updated_at)`},
	"comment": {"comments", "github_id", `json_object('number', number, 'author', author, 'body', body,
		'created_at', created_at, 'updated_at', updated_at)`},
	"review_comment": {"review_comments", "github_id", `json_object('number', number, 'in_reply_to', in_reply_to, 'author', author,
		'path', path, 'line', line, 'side', side, 'commit_id', commit_id, 'body', body, 'created_at', created_at, 'updated_at', updated_at)`},
}

// Known maps the ID of every synced item of kind in repoID to its issue or PR number.
func (s *Store) Known(ctx context.Context, repoID int64, kind string) (map[int64]int, error) {
	b := buried[kind]
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("SELECT %s, number FROM %s WHERE repo_id = ?", b.id, b.table), repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// Bury moves items of kind deleted on GitHub to tombstones, and drops tombstones older than keep.
func (s *Store) Bury(ctx context.Context, repoID int64, kind string, ids []int64, keep time.Duration) error {
	b := buried[kind]
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(fmt.Sprintf("INSERT INTO tombstones (repo_id, kind, item_id, item) SELECT repo_id, ?, %s, %s FROM %s WHERE repo_id = ? AND %s = ?",
				b.id, b.item, b.table, b.id), kind, repoID, id); err != nil {
				return err
			}
			if _, err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE repo_id = ? AND %s = ?", b.table, b.id), repoID, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec("DELETE FROM tombstones WHERE deleted_at < ?", time.Now().UTC().Add(-keep).Format(time.RFC3339Nano))
		return err
	})
}
