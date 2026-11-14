// Package store holds GitCove's SQLite database.
package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("not found")

type Store struct {
	db   *sql.DB
	path string
}

// Path is the database file, for processes such as Git hooks that open it themselves.
func (s *Store) Path() string { return s.path }

// Open opens or creates the database at path in WAL mode and applies pending migrations.
func Open(path string) (*Store, error) {
	// _txlock=immediate takes the write lock at BEGIN, so concurrent writers wait on busy_timeout instead of failing.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s := &Store{db, path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate applies migrations/NNNN_*.sql above PRAGMA user_version, each in one transaction.
func (s *Store) migrate() error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	var have int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&have); err != nil {
		return err
	}
	for i, name := range names { // fs.Glob returns sorted names
		if i < have {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = s.tx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(string(body)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func audit(tx *sql.Tx, actor, action, detail string) error {
	_, err := tx.Exec("INSERT INTO audit_log (actor, action, detail) VALUES (?, ?, ?)", actor, action, detail)
	return err
}

// Repo is a mirrored GitHub repository.
type Repo struct {
	ID            int64
	GitHubID      int64
	Owner, Name   string
	DefaultBranch string
	SyncedAt      string // last successful sync, empty if never
	SyncError     string // error of the last sync, empty if it succeeded
	Approvals     int    // approvals on GitCove a PR needs before a local merge (decision D4)
}

func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// UpsertRepo records a repo from GitHub, keyed by GitHub ID so renames follow, and returns its ID.
func (s *Store) UpsertRepo(ctx context.Context, githubID int64, owner, name, defaultBranch string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO repos (github_id, owner, name, default_branch) VALUES (?, ?, ?, ?)
		ON CONFLICT (github_id) DO UPDATE SET owner = excluded.owner, name = excluded.name, default_branch = excluded.default_branch
		RETURNING id`, githubID, owner, name, defaultBranch).Scan(&id)
	return id, err
}

// MarkSynced records the outcome of a repo's sync.
func (s *Store) MarkSynced(ctx context.Context, id int64, syncErr error) error {
	var err error
	if syncErr == nil {
		_, err = s.db.ExecContext(ctx, "UPDATE repos SET synced_at = ?, sync_error = NULL WHERE id = ?", now(), id)
	} else {
		_, err = s.db.ExecContext(ctx, "UPDATE repos SET sync_error = ? WHERE id = ?", syncErr.Error(), id)
	}
	return err
}

const repoCols = "id, github_id, owner, name, default_branch, coalesce(synced_at, ''), coalesce(sync_error, ''), approvals"

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	err := row.Scan(&r.ID, &r.GitHubID, &r.Owner, &r.Name, &r.DefaultBranch, &r.SyncedAt, &r.SyncError, &r.Approvals)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+repoCols+" FROM repos ORDER BY owner, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RepoByName(ctx context.Context, owner, name string) (Repo, error) {
	return scanRepo(s.db.QueryRowContext(ctx, "SELECT "+repoCols+" FROM repos WHERE owner = ? AND name = ?", owner, name))
}

// Token is a GitCove access token without its secret.
type Token struct {
	ID        int64
	Name      string
	CreatedAt string
	RevokedAt string
	Repos     []string // owner/name
	GitHub    string   // login of the GitHub account its holder authorised (decision D3), if any
}

func hashToken(plain string) []byte { h := sha256.Sum256([]byte(plain)); return h[:] }

// CreateToken issues a token with access to the given owner/name repos and returns its secret, shown only once.
func (s *Store) CreateToken(ctx context.Context, actor, name string, repos []string) (int64, string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	plain := "cove_" + base64.RawURLEncoding.EncodeToString(b)
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow("INSERT INTO tokens (name, hash) VALUES (?, ?) RETURNING id", name, hashToken(plain)).Scan(&id); err != nil {
			return err
		}
		for _, full := range repos {
			owner, repo, _ := strings.Cut(full, "/")
			res, err := tx.Exec("INSERT INTO token_repos (token_id, repo_id) SELECT ?, id FROM repos WHERE owner = ? AND name = ?", id, owner, repo)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("unknown repo %q (repos appear after the first sync)", full)
			}
		}
		return audit(tx, actor, "token.create", fmt.Sprintf("id=%d name=%q repos=%s", id, name, strings.Join(repos, ",")))
	})
	if err != nil {
		return 0, "", err
	}
	return id, plain, nil
}

func (s *Store) RevokeToken(ctx context.Context, actor string, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", now(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("token %d: %w or already revoked", id, ErrNotFound)
		}
		if _, err := tx.Exec("DELETE FROM user_tokens WHERE token_id = ?", id); err != nil {
			return err
		}
		return audit(tx, actor, "token.revoke", fmt.Sprintf("id=%d", id))
	})
}

func (s *Store) Tokens(ctx context.Context) ([]Token, error) { return s.tokens(ctx, "1", nil) }

// TokenFor returns plain's token, with its repos, if it is unrevoked.
func (s *Store) TokenFor(ctx context.Context, plain string) (Token, error) {
	toks, err := s.tokens(ctx, "t.hash = ? AND t.revoked_at IS NULL", hashToken(plain))
	if err != nil || len(toks) == 0 {
		return Token{}, cmp.Or(err, ErrNotFound)
	}
	return toks[0], nil
}

func (s *Store) tokens(ctx context.Context, where string, arg any) ([]Token, error) {
	var args []any
	if arg != nil {
		args = append(args, arg)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.name, t.created_at, coalesce(t.revoked_at, ''), coalesce(group_concat(r.owner || '/' || r.name, ','), ''),
			coalesce(u.login, '')
		FROM tokens t LEFT JOIN token_repos tr ON tr.token_id = t.id LEFT JOIN repos r ON r.id = tr.repo_id
		LEFT JOIN user_tokens u ON u.token_id = t.id
		WHERE `+where+` GROUP BY t.id ORDER BY t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var repos string
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.RevokedAt, &repos, &t.GitHub); err != nil {
			return nil, err
		}
		if repos != "" {
			t.Repos = strings.Split(repos, ",")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenValid reports whether plain is an unrevoked token.
func (s *Store) TokenValid(ctx context.Context, plain string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM tokens WHERE hash = ? AND revoked_at IS NULL", hashToken(plain)).Scan(&n)
	return n > 0, err
}

// TokenAccess returns the name of plain's token if it is unrevoked and has access to repo, else "".
func (s *Store) TokenAccess(ctx context.Context, plain string, repoID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `
		SELECT t.name FROM tokens t JOIN token_repos tr ON tr.token_id = t.id
		WHERE t.hash = ? AND t.revoked_at IS NULL AND tr.repo_id = ?`, hashToken(plain), repoID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// AuditEntry is one row of the audit log.
type AuditEntry struct{ At, Actor, Action, Detail string }

func (s *Store) Audit(ctx context.Context) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT at, actor, action, detail FROM audit_log ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
