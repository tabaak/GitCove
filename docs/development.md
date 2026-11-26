# Developing GitCove

```sh
go test ./...                                                        # includes the six fault campaigns on 10 seeds
COVE_SEEDS=40 go test -race ./internal/mirror ./internal/replay      # the nightly campaigns
```

`fakehub/` is a GitHub stand-in. It serves App authentication, repo and branch listing, Git smart HTTP including pushes, and PR creation. It refuses pushes the way GitHub refuses them from an App: protected branches, and workflow trees that match no branch tip. It logs every push, and it injects seeded faults: API or Git down, HTTP 500s, timeouts, pack responses cut off partway, writes carried out but never answered, slow answers, SSH down, and failing installation tokens. It also answers SSH greetings, for the health check. The campaign in `internal/mirror` runs rounds of random faults and GitHub-side changes (new commits, new and deleted branches, force-pushes). It checks every store with `git fsck` after each round, that one clean cycle afterwards brings every served ref level with GitHub, and that every tip GitCove ever served is still in a served ref or a history ref. `TestRestoreForcePushedBranch` restores a force-pushed branch, syncs straight after and again once history expires, and checks that no protocol version (also asked by hand, as a hostile client would) can fetch hidden history by commit ID.

`TestWebhookCampaign` makes GitHub-side changes (commits, force-pushes, deleted branches, new, edited and deleted issues and comments), each announced by a webhook that may be dropped, repeated, reordered or redelivered much later, plus forged ones, and syncs only what webhooks ask for. A repo whose webhooks got through must then match GitHub, except items deleted while every "deleted" webhook was dropped. Then a full sync must bring everything level, and a second must change nothing. `TestFullScanBuries` checks the scan buries deleted issues, comments and line comments and keeps the rest. `TestWebhookEndToEnd` sends a signed delivery to the real server, with syncs an hour apart, and checks that the commit arrives. To repeat a failing seed, run `COVE_SEEDS=N go test -run 'TestFaultCampaign/seed=K$' ./internal/mirror`.

The replay campaign in `internal/replay` (`TestReplayCampaign`) runs failovers in which developers push while GitHub is down and GitHub moves on. Meanwhile there are new commits, workflow changes, and developers pushing the same commits to GitHub themselves. Then it replays under faults, with GitCove killed at each crash point, GitHub moving between replay's fetch and its push, and up to three repeat outages with more pushes. After each run it checks:

- every queued push ended done or in conflicts
- every pushed commit is on GitHub, or kept on GitCove in a conflict
- there is one PR per fallback branch, and it's the one replay recorded
- commits already on GitHub got no PR
- GitHub refs moved only by fast-forward, and never on `main`
- a full sync agrees with GitHub, apart from branches held in conflicts

Removing any of the safety rules (no force, the already-on-GitHub check, the PR-only rule, the PR lookup, held branches) makes it fail.

The issue campaign in `internal/replay` (`TestIssueCampaign`) runs failovers in which two people, one with a linked GitHub account, open issues, comment, label, close, reopen and edit, mentioning new issues as `#L-n`, while GitHub changes the same issues. Then it replays with 500s, timeouts, lost responses, rate limits, listings that lag behind writes by up to three times the marker wait, GitCove killed before and after each write, and repeat outages. After each run it checks:

- every change ended done, in conflicts or uncertain, or waits on one that did
- nothing was posted twice, and every done post is on GitHub once
- a value GitHub's side set was replaced only by a change made against exactly that value
- posts appear under a person's name only for that person's changes, and otherwise carry the attribution line
- a full sync agrees with GitHub

Making replay resend after a restart, or apply edits without checking GitHub's version, makes it fail. `fakehub` also serves issues, comments and labels, the device flow, and user tokens whose refresh tokens rotate.

The PR campaign (`TestPullCampaign`) adds a third person and has them push, open PRs, review with line comments, reply, approve and merge locally, while on GitHub PR branches and `main` move, PRs are closed or merged, line comments are deleted and the linked account is revoked. Besides the checks above, it checks that replay never pushed to `main`, and that every approval or change request on GitHub came from the reviewer's own account, for their own review, at the commit they reviewed. `fakehub` enforces GitHub's rules here: a review's commit must be part of the PR, an approval must be for the PR's head and not by its author, and a reply needs its parent.

`TestDegradedCampaign` runs the replay and PR campaigns in degraded mode. GitHub is slow and flaky instead of down (up to 40% errors, timeouts, lost responses, rate limits, delays, lagging listings), and replay runs in the background, with kills, while people push, open PRs, review and merge. The campaigns' checks then apply unchanged. `TestUnlandedPushWaits` stops a push between the hook recording it and Git moving the branch, which is where background replay could otherwise drop it. The tests in `internal/health` use a fake clock against `fakehub`'s delay, flapping, SSH-only, HTTPS-only and installation-token faults. They check the alerts and their hints, and that the checks never change the mode. `TestFailoverEndToEnd` runs the real agent: it switches on failover, holds while GitCove restarts, switches back, follows `cove degraded start`, `pause` and `stop`, notifies on every switch when set up with `-notify`, and checks that each switch posts one team message. `TestAcceptedPushMessage` pushes through the real hook in failover, degraded and paused mode, and to the default branch, and checks the line the developer sees.

CI runs gofmt, `go vet`, golangci-lint, the tests with `-race`, and a Docker build. A nightly run uses 40 seeds.

Two scripts run the real binary against a sandbox organisation on GitHub (setup in [`spikes/README.md`](../spikes/README.md)). `spikes/sandbox.sh` runs a whole failover: pushes, a comment, an edit that conflicts with one on GitHub, a new issue, a PR that references it, a review with a line comment, a local merge, a diverged branch, and GitCove killed mid-replay. Then it checks on GitHub that everything arrived once and nothing was overwritten. `spikes/measure.sh` takes the measurements in [`docs/results.md`](results.md). Releasing is described in [`RELEASING.md`](../RELEASING.md).

## Code layout

```
cmd/cove/            server, admin CLI and developer agent in one binary
internal/ghclient/   GitHub API behind one interface: App JWT, installation tokens, ETags, rate limiter, write budget, device flow
internal/gitstore/   bare repos, fetch, served refs, history refs and restore, push, pre-receive hook, local merges, diffs, git http-backend
internal/store/      SQLite in WAL mode, migrations, mode, outbox, mirrored issues, PRs and reviews, webhook queue, tombstones, tokens, audit log
internal/mirror/     the sync loop (the plan's internal/sync): refs, branch protection, issues and comments, webhook-driven syncs, the full scan
internal/replay/     replay plan, history comparison, pushes and fallback PRs, replay of issues, comments, PRs, reviews and merges
internal/auth/       people's GitHub tokens: device flow, encryption, refresh
internal/health/     probes on four paths and alerts
internal/web/        HTTP: Git for developers, the web interface, /status, /webhook
fakehub/             fault-injecting GitHub stand-in
```
