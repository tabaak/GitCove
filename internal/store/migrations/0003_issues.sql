-- Issue and comment replay (plan milestone 3).

-- GitHub's issues and PRs as last synced. The issues listing returns both; is_pr marks PRs.
CREATE TABLE issues (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  number     INTEGER NOT NULL,
  is_pr      INTEGER NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL,
  state      TEXT NOT NULL,
  author     TEXT NOT NULL,
  labels     TEXT NOT NULL, -- JSON array of names
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, number)
);

-- Conversation comments on issues and PRs, as last synced.
CREATE TABLE comments (
  github_id  INTEGER PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  number     INTEGER NOT NULL,
  author     TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX comments_issue ON comments (repo_id, number, created_at);

CREATE TABLE labels (
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  name    TEXT NOT NULL,
  PRIMARY KEY (repo_id, name)
);

-- Items on issues: issue is the number or L-n acted on (for a new issue, its own L-n), token_id
-- who acted on GitCove, and poster the GitHub login replay posts as ('' for the App), fixed
-- when the item is marked sending, because the marker lookup depends on it.
ALTER TABLE outbox ADD COLUMN issue TEXT;
ALTER TABLE outbox ADD COLUMN token_id INTEGER REFERENCES tokens(id);
ALTER TABLE outbox ADD COLUMN poster TEXT;

-- Users' GitHub tokens (decision D3), encrypted with a key kept outside the database.
CREATE TABLE user_tokens (
  token_id           INTEGER PRIMARY KEY REFERENCES tokens(id),
  login              TEXT NOT NULL,
  access             BLOB NOT NULL,
  access_expires_at  TEXT NOT NULL,
  refresh            BLOB NOT NULL,
  refresh_expires_at TEXT NOT NULL
);
