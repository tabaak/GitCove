-- PRs, reviews and local merging (plan milestone 4).

-- GitHub's PRs as last synced, beside their row in issues, which holds the title, body and state.
CREATE TABLE pull_requests (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  number     INTEGER NOT NULL,
  head_ref   TEXT NOT NULL,
  base_ref   TEXT NOT NULL,
  fork       INTEGER NOT NULL, -- the head lives in another repo, so GitCove can't merge it
  merged     INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repo_id, number)
);

-- Review summaries and verdicts (state: APPROVED, CHANGES_REQUESTED, COMMENTED or DISMISSED).
CREATE TABLE reviews (
  github_id    INTEGER PRIMARY KEY,
  repo_id      INTEGER NOT NULL REFERENCES repos(id),
  number       INTEGER NOT NULL,
  author       TEXT NOT NULL,
  state        TEXT NOT NULL,
  body         TEXT NOT NULL,
  commit_id    TEXT NOT NULL,
  submitted_at TEXT NOT NULL
);
CREATE INDEX reviews_pr ON reviews (repo_id, number);

-- Line comments. A reply's in_reply_to is the first comment of its thread, so threads need no
-- table of their own (the plan's review_threads).
CREATE TABLE review_comments (
  github_id   INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id),
  number      INTEGER NOT NULL,
  in_reply_to INTEGER NOT NULL, -- 0 for the first comment of a thread
  author      TEXT NOT NULL,
  path        TEXT NOT NULL,
  line        INTEGER NOT NULL, -- 0 if GitHub no longer places it on a line
  side        TEXT NOT NULL,
  commit_id   TEXT NOT NULL,
  body        TEXT NOT NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX review_comments_pr ON review_comments (repo_id, number, created_at);

-- Approvals on GitCove a PR needs before a local merge (decision D4).
ALTER TABLE repos ADD COLUMN approvals INTEGER NOT NULL DEFAULT 1;
