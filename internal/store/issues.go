package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Issue is an issue or PR as GitHub last showed it.
type Issue struct {
	Number               int
	IsPR                 bool
	Title, Body, State   string
	Author               string
	Labels               []string
	CreatedAt, UpdatedAt string
}

// Comment is a conversation comment on an issue or PR as GitHub last showed it.
type Comment struct {
	ID                   int64
	Number               int
	Author, Body         string
	CreatedAt, UpdatedAt string
}

// Cursor returns where the named sync job got up to, "" if it never ran.
func (s *Store) Cursor(ctx context.Context, name string) (string, error) {
	var c string
	err := s.db.QueryRowContext(ctx, "SELECT cursor FROM sync_cursors WHERE name = ?", name).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// saveWithCursor runs f and moves the named cursor in one transaction, so a backfill resumes where it stopped.
func (s *Store) saveWithCursor(ctx context.Context, name, cursor string, f func(*sql.Tx) error) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := f(tx); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sync_cursors (name, cursor, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET cursor = excluded.cursor, updated_at = excluded.updated_at`, name, cursor, now())
		return err
	})
}

// SaveIssues upserts a page of issues and moves the cursor. A row never goes back to an older version.
func (s *Store) SaveIssues(ctx context.Context, repoID int64, cursorName, cursor string, issues []Issue) error {
	return s.saveWithCursor(ctx, cursorName, cursor, func(tx *sql.Tx) error {
		for _, i := range issues {
			labels, _ := json.Marshal(i.Labels)
			if _, err := tx.Exec(`INSERT INTO issues (repo_id, number, is_pr, title, body, state, author, labels, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (repo_id, number) DO UPDATE SET is_pr = excluded.is_pr, title = excluded.title, body = excluded.body,
				state = excluded.state, author = excluded.author, labels = excluded.labels, updated_at = excluded.updated_at
				WHERE excluded.updated_at >= issues.updated_at`,
				repoID, i.Number, i.IsPR, i.Title, i.Body, i.State, i.Author, labels, i.CreatedAt, i.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveComments upserts a page of comments and moves the cursor. A row never goes back to an older version.
func (s *Store) SaveComments(ctx context.Context, repoID int64, cursorName, cursor string, comments []Comment) error {
	return s.saveWithCursor(ctx, cursorName, cursor, func(tx *sql.Tx) error {
		for _, c := range comments {
			if _, err := tx.Exec(`INSERT INTO comments (github_id, repo_id, number, author, body, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (github_id) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at
				WHERE excluded.updated_at >= comments.updated_at`,
				c.ID, repoID, c.Number, c.Author, c.Body, c.CreatedAt, c.UpdatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetLabels replaces the repo's label list.
func (s *Store) SetLabels(ctx context.Context, repoID int64, names []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM labels WHERE repo_id = ?", repoID); err != nil {
			return err
		}
		for _, n := range names {
			if _, err := tx.Exec("INSERT OR IGNORE INTO labels (repo_id, name) VALUES (?, ?)", repoID, n); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Labels(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM labels WHERE repo_id = ? ORDER BY name", repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const issueCols = "number, is_pr, title, body, state, author, labels, created_at, updated_at"

func scanIssue(row interface{ Scan(...any) error }) (Issue, error) {
	var i Issue
	var labels string
	err := row.Scan(&i.Number, &i.IsPR, &i.Title, &i.Body, &i.State, &i.Author, &labels, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	} else if err != nil {
		return i, err
	}
	return i, json.Unmarshal([]byte(labels), &i.Labels)
}

// Issues lists the repo's issues and PRs, newest first.
func (s *Store) Issues(ctx context.Context, repoID int64) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+issueCols+" FROM issues WHERE repo_id = ? ORDER BY number DESC", repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) Issue(ctx context.Context, repoID int64, number int) (Issue, error) {
	return scanIssue(s.db.QueryRowContext(ctx, "SELECT "+issueCols+" FROM issues WHERE repo_id = ? AND number = ?", repoID, number))
}

// Comments lists an issue's comments, oldest first.
func (s *Store) Comments(ctx context.Context, repoID int64, number int) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT github_id, number, author, body, created_at, updated_at FROM comments
		WHERE repo_id = ? AND number = ? ORDER BY created_at, github_id`, repoID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Number, &c.Author, &c.Body, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Change is what a person did to an issue on GitCove. Fields by item kind: issue (Title, Body,
// Labels), comment (Body), label (Label, Add), state (State), edit (Field "title" or "body", Value).
type Change struct {
	Author string   `json:"author"` // the GitCove token's name
	Title  string   `json:"title,omitempty"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Label  string   `json:"label,omitempty"`
	Add    bool     `json:"add,omitempty"`
	State  string   `json:"state,omitempty"`
	Field  string   `json:"field,omitempty"`
	Value  string   `json:"value,omitempty"`
}

// Item is a queued change to an issue or PR (design 8.2).
type Item struct {
	ID               int64
	OpID             string
	RepoID           int64
	Repo             string // owner/name
	Kind             string // issue, comment, label, state or edit
	Issue            string // the number or L-n acted on; for kind issue, its own L-n
	TokenID          int64
	Base             string // for state and edit: GitHub's value the change was made against
	State            string // pending, sending, done, skipped, conflict or uncertain
	Target           string // fixed when sending: post, apply or note
	Poster           string // fixed when sending: the GitHub login posted as, "" for the App
	Result           string
	Created, Updated string
	Change           Change
}

// Record queues a change made on GitCove. It fails with ErrWrongMode unless the instance is in
// failover, checked in the same transaction. It returns the item's issue: for a new issue, its
// L-n. State and edit items record the value they change: the latest queued change of the same
// field, else the mirror's, so each change is replayed against the one before it.
func (s *Store) Record(ctx context.Context, repoID, tokenID int64, kind, issue string, c Change) (string, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Failover {
			return fmt.Errorf("%w: GitCove takes changes only during failover (mode is %s)", ErrWrongMode, m)
		}
		var base any
		switch kind {
		case "issue":
			var n int
			if err := tx.QueryRow("SELECT count(*) + 1 FROM outbox WHERE repo_id = ? AND kind = 'issue'", repoID).Scan(&n); err != nil {
				return err
			}
			issue = fmt.Sprintf("L-%d", n)
		case "state", "edit":
			b, err := currentValue(tx, repoID, issue, kind, c.Field)
			if err != nil {
				return err
			}
			base = b
		}
		payload, _ := json.Marshal(c)
		_, err := tx.Exec("INSERT INTO outbox (op_id, repo_id, kind, issue, token_id, base_version, payload) VALUES (?, ?, ?, ?, ?, ?, ?)",
			opID(), repoID, kind, issue, tokenID, base, payload)
		return err
	})
	return issue, err
}

func currentValue(tx *sql.Tx, repoID int64, issue, kind, field string) (string, error) {
	rows, err := tx.Query(`SELECT payload FROM outbox WHERE repo_id = ? AND issue = ? AND kind = ?
		AND state IN ('pending', 'sending') ORDER BY id DESC`, repoID, issue, kind)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var p []byte
		var c Change
		if err := rows.Scan(&p); err != nil {
			return "", err
		}
		if err := json.Unmarshal(p, &c); err != nil {
			return "", err
		}
		if kind == "state" {
			return c.State, nil
		}
		if c.Field == field {
			return c.Value, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if strings.HasPrefix(issue, "L-") {
		if kind == "edit" {
			return "", errors.New("a new issue is edited in place (EditLocal), not by an edit item")
		}
		return "open", nil
	}
	var state, title, body string
	err = tx.QueryRow("SELECT state, title, body FROM issues WHERE repo_id = ? AND number = ?", repoID, issue).Scan(&state, &title, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("issue #%s: %w", issue, ErrNotFound)
	}
	switch {
	case kind == "state":
		return state, err
	case field == "title":
		return title, err
	}
	return body, err
}

// EditLocal changes the title and body of a new issue that hasn't started replaying.
func (s *Store) EditLocal(ctx context.Context, repoID int64, localID, title, body string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Failover {
			return fmt.Errorf("%w: GitCove takes changes only during failover (mode is %s)", ErrWrongMode, m)
		}
		res, err := tx.Exec(`UPDATE outbox SET payload = json_set(payload, '$.title', ?, '$.body', ?), updated_at = ?
			WHERE repo_id = ? AND kind = 'issue' AND issue = ? AND state = 'pending'`, title, body, now(), repoID, localID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%s has already started replaying to GitHub; edit it there", localID)
		}
		return nil
	})
}

// MetaItems lists items on issues in the given states, in replay order (design 8.5): new issues,
// then comments, then labels and state, then edits, each oldest first. repoID 0 means every repo.
func (s *Store) MetaItems(ctx context.Context, repoID int64, states ...string) ([]Item, error) {
	args := []any{repoID, repoID}
	for _, st := range states {
		args = append(args, st)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, o.op_id, o.repo_id, r.owner || '/' || r.name, o.kind, coalesce(o.issue, ''),
		coalesce(o.token_id, 0), coalesce(o.base_version, ''), o.state, coalesce(o.target, ''), coalesce(o.poster, ''),
		coalesce(o.result, ''), o.created_at, o.updated_at, o.payload
		FROM outbox o JOIN repos r ON r.id = o.repo_id
		WHERE o.kind != 'ref' AND (? = 0 OR o.repo_id = ?) AND o.state IN (?`+strings.Repeat(", ?", len(states)-1)+`)
		ORDER BY CASE o.kind WHEN 'issue' THEN 2 WHEN 'comment' THEN 3 WHEN 'edit' THEN 5 ELSE 4 END, o.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var p []byte
		if err := rows.Scan(&it.ID, &it.OpID, &it.RepoID, &it.Repo, &it.Kind, &it.Issue, &it.TokenID, &it.Base, &it.State,
			&it.Target, &it.Poster, &it.Result, &it.Created, &it.Updated, &p); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(p, &it.Change); err != nil {
			return nil, fmt.Errorf("outbox %d: %w", it.ID, err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkMeta moves a pending or sending item to sending with what replay is about to do and as
// whom, only while replaying. It's written before any request, so a restart knows a write may
// have landed and where to look for it.
func (s *Store) MarkMeta(ctx context.Context, id int64, target, poster string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if m, err := mode(tx); err != nil {
			return err
		} else if m != Replaying {
			return fmt.Errorf("%w: mode is %s", ErrWrongMode, m)
		}
		return updateItems(tx, []int64{id}, "state = 'sending', target = ?, poster = ?", target, poster)
	})
}

// Requeue returns a sending item to pending, for a request GitHub refused without carrying it out.
func (s *Store) Requeue(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return updateItems(tx, []int64{id}, "state = 'pending'") })
}

// FinishIssue ends a new issue's item as done and maps its L-n to the GitHub number, in one transaction.
func (s *Store) FinishIssue(ctx context.Context, it Item, number int) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := updateItems(tx, []int64{it.ID}, "state = 'done', result = ?", fmt.Sprintf("#%d", number)); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT OR IGNORE INTO id_map (repo_id, kind, local_id, github_number) VALUES (?, 'issue', ?, ?)",
			it.RepoID, it.Issue, number)
		return err
	})
}

// MappedIssue returns the GitHub number of new issue localID, 0 if it has none yet, and the
// state of its item ("" if there's no such issue).
func (s *Store) MappedIssue(ctx context.Context, repoID int64, localID string) (int, string, error) {
	var n int
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT
		coalesce((SELECT github_number FROM id_map WHERE repo_id = ? AND kind = 'issue' AND local_id = ?), 0),
		coalesce((SELECT state FROM outbox WHERE repo_id = ? AND kind = 'issue' AND issue = ?), '')`,
		repoID, localID, repoID, localID).Scan(&n, &state)
	return n, state, err
}

// UserToken is a person's GitHub user token (decision D3), encrypted by package auth.
type UserToken struct {
	TokenID                       int64
	Login                         string
	Access, Refresh               []byte
	AccessExpires, RefreshExpires string // RFC 3339
}

func (s *Store) UserToken(ctx context.Context, tokenID int64) (UserToken, error) {
	u := UserToken{TokenID: tokenID}
	err := s.db.QueryRowContext(ctx, `SELECT login, access, access_expires_at, refresh, refresh_expires_at FROM user_tokens
		WHERE token_id = ?`, tokenID).Scan(&u.Login, &u.Access, &u.AccessExpires, &u.Refresh, &u.RefreshExpires)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return u, err
}

// SaveUserToken stores u and audits it as action (user_token.authorise or user_token.refresh).
func (s *Store) SaveUserToken(ctx context.Context, actor, action string, u UserToken) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO user_tokens (token_id, login, access, access_expires_at, refresh, refresh_expires_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (token_id) DO UPDATE SET login = excluded.login, access = excluded.access,
			access_expires_at = excluded.access_expires_at, refresh = excluded.refresh, refresh_expires_at = excluded.refresh_expires_at`,
			u.TokenID, u.Login, u.Access, u.AccessExpires, u.Refresh, u.RefreshExpires); err != nil {
			return err
		}
		return audit(tx, actor, action, fmt.Sprintf("token=%d login=%s", u.TokenID, u.Login))
	})
}

// DropUserToken forgets a person's GitHub token, so their work is posted by the App.
func (s *Store) DropUserToken(ctx context.Context, actor string, tokenID int64, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM user_tokens WHERE token_id = ?", tokenID); err != nil {
			return err
		}
		return audit(tx, actor, "user_token.drop", fmt.Sprintf("token=%d: %s", tokenID, reason))
	})
}
