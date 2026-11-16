-- Backup protections and sync hardening (plan milestone 6).
-- Webhook deliveries already queued, so a redelivery, which reuses X-GitHub-Delivery, is ignored.
CREATE TABLE webhook_deliveries (
  id          TEXT PRIMARY KEY,
  received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX webhook_deliveries_age ON webhook_deliveries (received_at);
-- Set by a webhook, cleared when a sync of the repo starts: the durable queue webhooks feed.
ALTER TABLE repos ADD COLUMN sync_wanted INTEGER NOT NULL DEFAULT 0;
-- Items the full scan found deleted on GitHub, kept until the retention window ends.
CREATE TABLE tombstones (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  kind       TEXT NOT NULL,    -- issue, comment or review_comment
  item_id    INTEGER NOT NULL, -- the issue number or comment ID
  item       TEXT NOT NULL,    -- the last synced version, as JSON
  deleted_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
