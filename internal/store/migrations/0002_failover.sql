-- Code failover and replay (plan milestone 2).

-- Instance-wide settings: 'mode' (normal, failover, reconciling, replaying) and 'allow_protected'.
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO settings (key, value) VALUES ('mode', 'normal'), ('allow_protected', '0');

-- GitHub's `protected` flag per branch (classic protection or any active ruleset, spike A),
-- replaced on each sync. branches_synced_at is NULL until the first listing succeeds.
CREATE TABLE github_branches (
  repo_id   INTEGER NOT NULL REFERENCES repos(id),
  branch    TEXT NOT NULL,
  protected INTEGER NOT NULL,
  PRIMARY KEY (repo_id, branch)
);
ALTER TABLE repos ADD COLUMN branches_synced_at TEXT;

-- Default and deploy branches reach GitHub only through a PR unless an admin turns this off.
ALTER TABLE repos ADD COLUMN pr_only INTEGER NOT NULL DEFAULT 1;

-- Admin-listed branches: 'protected' (extra to GitHub's) or 'deploy' (PR-only on replay).
CREATE TABLE branch_marks (
  repo_id INTEGER NOT NULL REFERENCES repos(id),
  branch  TEXT NOT NULL,
  kind    TEXT NOT NULL CHECK (kind IN ('protected', 'deploy')),
  PRIMARY KEY (repo_id, branch, kind)
);

-- target: the GitHub branch replay pushes to, fixed when the item is marked sending.
-- result: what replay did, e.g. 'fast-forward' or 'PR #12'.
ALTER TABLE outbox ADD COLUMN target TEXT;
ALTER TABLE outbox ADD COLUMN result TEXT;
CREATE INDEX outbox_state ON outbox (state, repo_id);
