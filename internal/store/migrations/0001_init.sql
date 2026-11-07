-- Initial schema (plan milestone 1). Times are UTC RFC 3339 text.

CREATE TABLE repos (
  id             INTEGER PRIMARY KEY,
  github_id      INTEGER NOT NULL UNIQUE,
  owner          TEXT NOT NULL,
  name           TEXT NOT NULL,
  default_branch TEXT NOT NULL DEFAULT '',
  synced_at      TEXT,
  sync_error     TEXT,
  UNIQUE (owner, name)
);

CREATE TABLE actors (
  id        INTEGER PRIMARY KEY,
  github_id INTEGER UNIQUE,
  login     TEXT NOT NULL UNIQUE
);

-- Append-only record of what arrived from GitHub or local users.
CREATE TABLE events (
  id          INTEGER PRIMARY KEY,
  source      TEXT NOT NULL,
  kind        TEXT NOT NULL,
  repo_id     INTEGER REFERENCES repos(id),
  payload     BLOB NOT NULL,
  hash        TEXT NOT NULL,
  received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Queued failover actions (section 8.2).
CREATE TABLE outbox (
  id           INTEGER PRIMARY KEY,
  op_id        TEXT NOT NULL UNIQUE,
  repo_id      INTEGER NOT NULL REFERENCES repos(id),
  kind         TEXT NOT NULL,
  state        TEXT NOT NULL DEFAULT 'pending'
               CHECK (state IN ('pending', 'sending', 'done', 'skipped', 'conflict', 'uncertain')),
  base_version TEXT,
  payload      BLOB NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- Temporary L-n IDs mapped to GitHub numbers.
CREATE TABLE id_map (
  repo_id       INTEGER NOT NULL REFERENCES repos(id),
  kind          TEXT NOT NULL,
  local_id      TEXT NOT NULL,
  github_number INTEGER NOT NULL,
  PRIMARY KEY (repo_id, kind, local_id)
);

-- Anything needing a human, with both versions.
CREATE TABLE conflicts (
  id             INTEGER PRIMARY KEY,
  outbox_id      INTEGER REFERENCES outbox(id),
  reason         TEXT NOT NULL,
  local_version  BLOB,
  github_version BLOB,
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  resolved_at    TEXT
);

-- Where each sync job got up to; name is e.g. 'repo:12:issues'.
CREATE TABLE sync_cursors (
  name       TEXT PRIMARY KEY,
  cursor     TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- GitCove access tokens; only the SHA-256 of the token is stored.
CREATE TABLE tokens (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  hash       BLOB NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  revoked_at TEXT
);

-- Repo access per token, assigned by an admin.
CREATE TABLE token_repos (
  token_id INTEGER NOT NULL REFERENCES tokens(id),
  repo_id  INTEGER NOT NULL REFERENCES repos(id),
  PRIMARY KEY (token_id, repo_id)
);

CREATE TABLE audit_log (
  id     INTEGER PRIMARY KEY,
  at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  actor  TEXT NOT NULL,
  action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
