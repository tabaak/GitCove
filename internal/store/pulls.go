package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// PullRequest is what GitHub last showed of a PR beyond its row in issues.
type PullRequest struct {
	Number           int
	HeadRef, BaseRef string
	Fork, Merged     bool
	UpdatedAt        string
}

// Review is a review's summary and verdict as GitHub last showed it.
type Review struct {
	ID                  int64
	Number              int
	Author, State, Body string
	Commit, SubmittedAt string
}

// ReviewComment is a line comment as GitHub last showed it. InReplyTo is 0 for the first of a thread.
type ReviewComment struct {
	ID, InReplyTo        int64
	Number               int
	Author, Path         string
	Line                 int
	Side, Commit, Body   string
	CreatedAt, UpdatedAt string
}

// SavePulls upserts PRs. A row never goes back to an older version.
func (s *Store) SavePulls(ctx context.Context, repoID int64, prs []PullRequest) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, p := range prs {
			if _, err := tx.Exec(`INSERT INTO pull_requests (repo_id, number, head_ref, base_ref, fork, merged, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (repo_id, number) DO UPDATE SET head_ref = excluded.head_ref, base_ref = excluded.base_ref,
				fork = excluded.fork, merged = excluded.merged, updated_at = excluded.updated_at
				WHERE excluded.updated_at >= pull_requests.updated_at`,
				repoID, p.Number, p.HeadRef, p.BaseRef, p.Fork, p.Merged, p.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetCursor moves the named sync cursor by itself, for listings saved before their cursor is known.
func (s *Store) SetCursor(ctx context.Context, name, cursor string) error {
	return s.saveWithCursor(ctx, name, cursor, func(*sql.Tx) error { return nil })
}

func (s *Store) Pull(ctx context.Context, repoID int64, number int) (PullRequest, error) {
	p := PullRequest{Number: number}
	err := s.db.QueryRowContext(ctx, "SELECT head_ref, base_ref, fork, merged, updated_at FROM pull_requests WHERE repo_id = ? AND number = ?",
		repoID, number).Scan(&p.HeadRef, &p.BaseRef, &p.Fork, &p.Merged, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return p, err
}

// SetReviews replaces a PR's reviews.
func (s *Store) SetReviews(ctx context.Context, repoID int64, number int, reviews []Review) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM reviews WHERE repo_id = ? AND number = ?", repoID, number); err != nil {
			return err
		}
		for _, r := range reviews {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO reviews (github_id, repo_id, number, author, state, body, commit_id, submitted_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, repoID, number, r.Author, r.State, r.Body, r.Commit, r.SubmittedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reviews lists a PR's reviews, oldest first.
func (s *Store) Reviews(ctx context.Context, repoID int64, number int) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT github_id, number, author, state, body, commit_id, submitted_at FROM reviews
		WHERE repo_id = ? AND number = ? ORDER BY submitted_at, github_id`, repoID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.Number, &r.Author, &r.State, &r.Body, &r.Commit, &r.SubmittedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveReviewComments upserts a page of line comments and moves the cursor.
func (s *Store) SaveReviewComments(ctx context.Context, repoID int64, cursorName, cursor string, comments []ReviewComment) error {
	return s.saveWithCursor(ctx, cursorName, cursor, func(tx *sql.Tx) error {
		for _, c := range comments {
			if _, err := tx.Exec(`INSERT INTO review_comments (github_id, repo_id, number, in_reply_to, author, path, line, side, commit_id, body, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (github_id) DO UPDATE SET body = excluded.body, line = excluded.line, updated_at = excluded.updated_at
				WHERE excluded.updated_at >= review_comments.updated_at`,
				c.ID, repoID, c.Number, c.InReplyTo, c.Author, c.Path, c.Line, c.Side, c.Commit, c.Body, c.CreatedAt, c.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReviewComments lists a PR's line comments, oldest first.
func (s *Store) ReviewComments(ctx context.Context, repoID int64, number int) ([]ReviewComment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT github_id, in_reply_to, number, author, path, line, side, commit_id, body, created_at, updated_at
		FROM review_comments WHERE repo_id = ? AND number = ? ORDER BY created_at, github_id`, repoID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewComment
	for rows.Next() {
		var c ReviewComment
		if err := rows.Scan(&c.ID, &c.InReplyTo, &c.Number, &c.Author, &c.Path, &c.Line, &c.Side, &c.Commit, &c.Body, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetApprovals sets how many approvals on GitCove a PR in repo needs before a local merge.
func (s *Store) SetApprovals(ctx context.Context, actor string, repo Repo, n int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE repos SET approvals = ? WHERE id = ?", n, repo.ID); err != nil {
			return err
		}
		return audit(tx, actor, "repo.approvals", fmt.Sprintf("%s %d", repo.FullName(), n))
	})
}

// RecordMerge queues a local merge of PR ref and, in the same transaction, the closing of the
// issues its text says it fixes. It returns the new items' IDs, for Unrecord.
func (s *Store) RecordMerge(ctx context.Context, repoID, tokenID int64, ref string, merge Change, closes []string) ([]int64, error) {
	var ids []int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, id, err := record(tx, repoID, tokenID, "merge", ref, merge)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		for _, issue := range closes {
			_, id, err := record(tx, repoID, tokenID, "state", issue, Change{Author: merge.Author, State: "closed", Via: ref})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// Unrecord removes items just recorded whose action then failed on GitCove, such as a merge
// whose branch moved underneath it. Only items replay hasn't touched.
func (s *Store) Unrecord(ctx context.Context, ids []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec("DELETE FROM outbox WHERE id = ? AND state = 'pending'", id); err != nil {
				return err
			}
		}
		return nil
	})
}

// PushedHead says where on GitHub the work pushed to branch during failover is: branch itself,
// or the gitcove/ fallback branch replay sent it to. waits is the state of a push to it that
// hasn't reached GitHub, "" if there's none.
func (s *Store) PushedHead(ctx context.Context, repoID int64, branch string) (head, waits string, err error) {
	items, err := s.RefItems(ctx, repoID, "pending", "sending", "conflict", "uncertain", "done")
	head = branch
	for _, it := range items {
		switch {
		case it.Branch != branch:
		case it.State != "done":
			waits = it.State
		case it.Target != "":
			head = it.Target
		}
	}
	return head, waits, err
}

// MapReviewComments records the GitHub IDs of a replayed review's line comments, by index.
func (s *Store) MapReviewComments(ctx context.Context, it Item, ids map[int]int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.Exec("INSERT OR IGNORE INTO id_map (repo_id, kind, local_id, github_number) VALUES (?, 'review_comment', ?, ?)",
				it.RepoID, fmt.Sprintf("%s/%d", it.OpID, i), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// LocalComment resolves a reply's parent "<review's op ID>/<index>": the review item, and the
// line comment's GitHub ID, 0 if it has none (yet, or because the review wasn't posted as one).
func (s *Store) LocalComment(ctx context.Context, repoID int64, parent string) (Item, int64, error) {
	op, _, _ := strings.Cut(parent, "/")
	items, err := s.MetaItems(ctx, repoID, "pending", "sending", "done", "skipped", "conflict", "uncertain")
	if err != nil {
		return Item{}, 0, err
	}
	for _, it := range items {
		if it.OpID == op && it.Kind == "review" {
			var id int64
			err := s.db.QueryRowContext(ctx, "SELECT github_number FROM id_map WHERE repo_id = ? AND kind = 'review_comment' AND local_id = ?",
				repoID, parent).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			return it, id, err
		}
	}
	return Item{}, 0, ErrNotFound
}
