# GitCove

A self-hosted mirror of a GitHub organisation that developers can keep working on while GitHub is down, and that replays their work to GitHub afterwards. The build plan is in [`plan.md`](plan.md).

**Status: milestone 2 (code failover and replay).** GitCove mirrors every repo its GitHub App can see and serves clones and fetches to developers holding a GitCove token, including while GitHub is unreachable. During a failover that an admin starts, developers push branches to GitCove. Afterwards GitCove replays them to GitHub: it never force-pushes, and work for the default branch arrives only as a PR. Issues, comments and PRs on GitCove come in milestones 3 and 4.

GitCove keeps a second copy of your private code. Protect the server and its disk the way you protect anything that holds source code.

## Run it

You need a GitHub App installed on the organisation, with **Contents: read and write**, **Pull requests: read and write** and **Metadata: read**, and its private key. Replay needs the write permissions: it pushes branches and opens PRs. Don't grant **Workflows**. Without it, GitHub refuses a pushed branch whose `.github/workflows` matches no current branch tip. GitCove checks this before pushing and sends such branches to the conflicts list (see below). A key that has Workflows could change CI and reach its secrets.

```sh
docker build -t gitcove .
docker run -d --name gitcove -p 8080:8080 -v gitcove-data:/data \
  -e COVE_ORG=my-org -e COVE_APP_ID=123456 -e COVE_APP_KEY_FILE=/data/app.pem \
  gitcove
```

Or without Docker, with Go 1.26 and `git` installed: `go build ./cmd/cove && ./cove serve`.

Put TLS in front of it, for example with Caddy. Developers send their token as an HTTP password, so plain HTTP exposes it. GitCove can also serve TLS itself, using `COVE_TLS_CERT` and `COVE_TLS_KEY`.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `COVE_ORG` | required | GitHub organisation |
| `COVE_APP_ID` | required | GitHub App ID |
| `COVE_APP_KEY_FILE` | required | Path to the App's private key (PEM) |
| `COVE_DATA` | `./data` | Database and bare repos |
| `COVE_LISTEN` | `:8080` | HTTP listen address |
| `COVE_TLS_CERT`, `COVE_TLS_KEY` | unset | Serve HTTPS directly |
| `COVE_SYNC_INTERVAL` | `5m` | Time between sync cycles |
| `COVE_HEALTH_INTERVAL` | `30s` | Time between GitHub health probes |
| `COVE_FETCH_TIMEOUT` | `10m` | Longest time a single repo fetch or replay push may take |
| `COVE_REPLAY_INTERVAL` | `30s` | Time between replay passes while replaying |
| `COVE_ALERT_WEBHOOK` | unset | URL that admin alerts are POSTed to as `{"text": ...}` (Slack-style). Alerts always go to the log too |
| `COVE_GITHUB_API`, `COVE_GITHUB_GIT` | `https://api.github.com`, `https://github.com` | GitHub endpoints, overridden only in tests |

### Admin CLI

The CLI works on the database in `COVE_DATA` directly, so run it on the server (inside the container, use `docker exec gitcove cove ...`).

```sh
cove sync                                                 # mirror every repo once
cove token create -name alice -repo my-org/api -repo my-org/web   # prints the token once
cove token list
cove token revoke 3
```

Tokens are stored only as SHA-256 hashes. An admin assigns each token's repos by hand, and a repo can be assigned only after its first sync. Token creation and revocation are written to the audit log. A token that can read a repo can also push to it during failover. There is no separate write permission yet, and permission sync from GitHub comes in milestone 7.

### Failover and replay

```sh
cove failover start                       # GitHub is down: GitCove accepts pushes
cove failover allow-protected on          # emergency only: allow direct pushes to protected branches
cove failover end                         # GitHub is back: refuse pushes, catch up, print the replay plan
cove replay plan                          # the dry run again, at any time
cove replay approve                       # the server replays the plan
cove replay skip 12                       # drop a conflict for good
cove replay retry 12                      # queue it again (then `cove replay approve`)
cove branch mark my-org/api prod deploy   # deploy branches get PRs too
cove branch mark my-org/api rc protected  # protected on GitCove even if not on GitHub
cove branch list
cove repo pr-only my-org/api off          # for repos with no push-based deploys
```

The mode (normal, failover, reconciling, replaying) lives in the database, so it survives a restart, and every change is written to the audit log. So are the emergency setting, branch marks, the PR-only setting, and skips and retries. `GET /status` shows the mode.

**During failover** GitCove accepts pushes to branches. It refuses:

- pushes outside failover
- tags, and deleting branches
- force-pushes
- pushes to protected branches. The refusal explains how to push to a new branch instead.

A branch is protected if GitHub's branch listing says so (classic protection, or any active repo or organisation ruleset) or an admin marked it. GitCove is no stricter than GitHub, so an unprotected default branch accepts pushes. If GitCove doesn't know a branch's protection, it treats the branch as protected. That covers a repo whose branches were never listed, and a branch created on GitHub since the last listing. A new branch name, one GitHub doesn't have yet, is accepted. If a GitHub ruleset pattern refuses it at replay, it goes to the conflicts list with its commits kept.

Each accepted push is queued for replay before Git moves the ref, so a crash can't lose it. Until its work reaches GitHub, a branch with queued pushes stays where developers left it, and other branches keep following GitHub.

**Replay** fetches each repo again before pushing, then for each branch:

1. If GitHub already has the commits on any branch (someone pushed them there themselves), it pushes nothing.
2. For the default branch, a deploy branch or a protected branch, it pushes `gitcove/<branch>-<timestamp>` and opens a PR into the branch. This is the PR-only rule. It stops replay from triggering a push-based deploy of code that never ran CI.
3. If GitHub's branch is behind GitCove's, it pushes, which is a fast-forward. A branch GitHub doesn't have is created.
4. Otherwise the branches have diverged, and it pushes a fallback branch and opens a PR, as in step 2.

Before pushing anything, replay compares the branch's `.github/workflows` with every branch tip on GitHub. If none matches, GitHub would refuse the push, so the branch goes to the conflicts list with the reason. The developer can merge the current target into it and push it to GitHub themselves, and then `cove replay retry` finds it already there.

Pushes never use `--force`, so GitHub only ever fast-forwards. Each push can start workflows on GitHub, so the plan lists every branch it will push. Once a branch's work is on GitHub, the branch follows GitHub again, even while the PR is unmerged. Until then, the unmerged work is on the `gitcove/...` branch, as it would be on GitHub. A branch in the conflicts list keeps its GitCove commits until an admin skips it.

Replay can stop at any point and resume. The fallback branch name is stored before anything is sent. A restart looks up the PR from that branch before opening one, and GitHub refuses a second open PR with the same head and base. If GitHub fails during replay, replay pauses, raises an alert, and retries on each interval. `cove failover start` returns to failover. Finished items stay finished, and new pushes join the queue.

### For developers

Once, while things are normal (Git needs a credential helper, such as `osxkeychain` or `libsecret`):

```sh
COVE_TOKEN=<token> cove client setup -url https://gitcove.example.com -org my-org
```

When failover starts and ends:

```sh
cove client on    # Git sends github.com/my-org/... (HTTPS and SSH URLs) to GitCove; existing clones work unchanged
cove client off   # back to GitHub
```

This only changes the `url.<gitcove>/my-org/.insteadOf` rule in your global Git config. Without the CLI, `git clone https://x:<token>@gitcove.example.com/my-org/api.git` works too. Any username works, and the token is the password.

### Status

`GET /status` is public and returns the mode, repo counts, the oldest sync time (the age of the copy), and whether GitHub's API and Git hosting answered the last probe. It shows no repo names or error text.

## How the mirror works

- Each repo is a bare repository named by its GitHub ID, so a rename keeps it.
- A fetch writes only GitHub's copy of the refs, under `refs/gitcove/github/`. The refs developers see (`refs/heads`, `refs/tags`, `refs/pull`) are then moved to match in one atomic compare-and-swap update. Branches with queued or conflicted pushes are left alone. A fetch that fails or is cut off changes nothing, and the next cycle retries it.
- Each sync also lists each repo's branches to record GitHub's `protected` flag.
- Pushes go through a `pre-receive` hook (`cove hook pre-receive`) installed outside every repo. Clients can't write `refs/gitcove/`.
- `refs/gitcove/` is hidden from clients' ref listings. Whether a client can still fetch a hidden commit by its ID is tested in milestone 7.
- The installation token reaches `git` through the environment, never the command line.
- The health check makes an API call (`/rate_limit`, which doesn't count against the limit) and an unauthenticated Git request. Any Git answer below 500 counts as up.

## Development

```sh
go test ./...                                                        # includes both fault campaigns on 10 seeds
COVE_SEEDS=200 go test -race ./internal/mirror ./internal/replay     # the nightly campaigns
```

`fakehub/` is a GitHub stand-in. It serves App authentication, repo and branch listing, Git smart HTTP including pushes, and PR creation. It refuses pushes the way GitHub refuses them from an App: protected branches, and workflow trees that match no branch tip. It logs every push, and it injects seeded faults: API or Git down, HTTP 500s, timeouts, pack responses cut off partway, and writes carried out but never answered. The campaign in `internal/mirror` runs rounds of random faults and GitHub-side changes (new commits, new and deleted branches, force-pushes). It checks every store with `git fsck` after each round, and checks that one clean cycle afterwards brings every served ref level with GitHub. To repeat a failing seed, run `COVE_SEEDS=N go test -run 'TestFaultCampaign/seed=K$' ./internal/mirror`.

The replay campaign in `internal/replay` (`TestReplayCampaign`) runs failovers in which developers push while GitHub is down and GitHub moves on. Meanwhile there are new commits, workflow changes, and developers pushing the same commits to GitHub themselves. Then it replays under faults, with GitCove killed at each crash point, GitHub moving between replay's fetch and its push, and up to three repeat outages with more pushes. After each run it checks:

- every queued push ended done or in conflicts
- every pushed commit is on GitHub, or kept on GitCove in a conflict
- there is one PR per fallback branch, and it's the one replay recorded
- commits already on GitHub got no PR
- GitHub refs moved only by fast-forward, and never on `main`
- a full sync agrees with GitHub, apart from branches held in conflicts

Removing any of the safety rules (no force, the already-on-GitHub check, the PR-only rule, the PR lookup, held branches) makes it fail.

CI runs gofmt, `go vet`, golangci-lint, the tests with `-race`, and a Docker build. A nightly run uses 200 seeds.

## Code layout

```
cmd/cove/            server and admin CLI in one binary
internal/ghclient/   GitHub API behind one interface: App JWT, installation tokens, ETags, rate limiter
internal/gitstore/   bare repos, fetch, served refs, push, pre-receive hook, git http-backend
internal/store/      SQLite in WAL mode, migrations, mode, outbox, tokens, audit log
internal/mirror/     the sync loop (the plan's internal/sync), branch protection
internal/replay/     replay plan, history comparison, pushes and fallback PRs
internal/health/     API and Git probes
internal/web/        HTTP: Git for developers, /status
fakehub/             fault-injecting GitHub stand-in
```

## Licence

Apache 2.0.
