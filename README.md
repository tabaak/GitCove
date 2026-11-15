# GitCove

A self-hosted mirror of a GitHub organisation that developers can keep working on while GitHub is down, and that replays their work to GitHub afterwards. The build plan is in [`plan.md`](plan.md).

**Status: milestone 3 (issue and comment replay).** GitCove mirrors every repo its GitHub App can see, with its issues, PRs and comments, and serves them to developers holding a GitCove token, including while GitHub is unreachable. During a failover that an admin starts, developers push branches to GitCove, and in its web interface they comment, open issues, label, close, reopen and edit. Afterwards GitCove replays that work to GitHub. It never force-pushes, work for the default branch arrives only as a PR, nothing is posted twice, and nothing newer on GitHub is overwritten. Opening PRs, reviews and merging on GitCove come in milestone 4.

GitCove keeps a second copy of your private code, issues and comments. Protect the server and its disk the way you protect anything that holds source code.

## Run it

You need a GitHub App installed on the organisation, with **Contents: read and write**, **Issues: read and write**, **Pull requests: read and write** and **Metadata: read**, and its private key. Replay needs the write permissions: it pushes branches, opens PRs, and posts issues and comments. Don't grant **Workflows**. Without it, GitHub refuses a pushed branch whose `.github/workflows` matches no current branch tip. GitCove checks this before pushing and sends such branches to the conflicts list (see below). A key that has Workflows could change CI and reach its secrets.

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
| `COVE_APP_CLIENT_ID` | unset | The App's client ID. With the next two, lets people post their outage work under their own names (see below) |
| `COVE_APP_CLIENT_SECRET_FILE` | unset | Path to a file holding the App's client secret |
| `COVE_USER_KEY_FILE` | unset | Path to the key that encrypts people's GitHub tokens: 64 hex digits, e.g. from `openssl rand -hex 32`. Keep it off the data volume |
| `COVE_GITHUB_SSH` | `github.com:22` | GitHub's SSH server, for the health check. `off` skips it, if GitCove's server can't reach that port at all |
| `COVE_GITHUB_API`, `COVE_GITHUB_GIT` | `https://api.github.com`, `https://github.com` | GitHub endpoints, overridden only in tests |

### Admin CLI

The CLI works on the database in `COVE_DATA` directly, so run it on the server (inside the container, use `docker exec gitcove cove ...`).

```sh
cove sync                                                 # mirror every repo once
cove token create -name alice -repo my-org/api -repo my-org/web   # prints the token once
cove token list
cove token revoke 3
```

Tokens are stored only as SHA-256 hashes. An admin assigns each token's repos by hand, and a repo can be assigned only after its first sync. Token creation and revocation are written to the audit log. A token that can read a repo can also push to it and change its issues during failover. There is no separate write permission yet, and permission sync from GitHub comes in milestone 7.

### Failover and replay

```sh
cove failover start                       # GitHub is down: GitCove accepts pushes
cove failover allow-protected on          # emergency only: allow direct pushes to protected branches
cove failover end                         # GitHub is back: refuse pushes, catch up, print the replay plan
cove replay plan                          # the dry run again, at any time
cove replay approve                       # the server replays the plan
cove replay skip 12                       # drop a conflict for good
cove replay retry 12                      # queue it again (then `cove replay approve`)
cove degraded start                       # GitHub is slow or flaky: take work, replay it in the background
cove degraded pause                       # keep taking work, stop replaying it (`degraded start` resumes)
cove degraded stop                        # refuse pushes again; what's left replays, then back to normal
cove branch mark my-org/api prod deploy   # deploy branches get PRs too
cove branch mark my-org/api rc protected  # protected on GitCove even if not on GitHub
cove branch list
cove repo pr-only my-org/api off          # for repos with no push-based deploys
```

The mode (normal, failover, reconciling, replaying, degraded, degraded-paused) lives in the database, so it survives a restart, and every change is written to the audit log. So are the emergency setting, branch marks, the PR-only setting, and skips and retries. `GET /status` shows the mode.

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

### Health checks and degraded mode

Every `COVE_HEALTH_INTERVAL`, GitCove checks GitHub on four paths at once: the API signed in as the App, the API without signing in, Git over HTTPS, and Git over SSH (only its greeting, so no key is needed). An answer slower than 5 seconds counts as an error. GitCove doesn't test writes, because a test write would leave something on GitHub each time. Replay's own writes show whether writing works.

When a path hasn't answered for 2 minutes, the admin gets an alert naming it, and another when the set changes or everything answers again. If only one of SSH and HTTPS is broken, the alert gives the `insteadOf` rule that moves developers to the other. If every path is down, it says to run `cove failover start`. If only the signed-in API fails, the alert shows that too, but it's GitCove's problem (the App or its key), not an outage. When 20% of the last 5 minutes' checks failed or were slow, an alert says GitHub is slow or flaky and suggests `cove degraded start`; once every path has answered in time for 10 minutes, another says so and, in degraded mode, suggests `cove degraded stop` (and, while paused, `cove degraded start` to resume). Paths that are fully down don't count as flaky, since they get their own alert. These thresholds are guesses (decision D6).

GitCove never switches modes by itself: only the admin does. If it switched by mistake, the team's work would be split across two systems, and for most teams that costs more than a few minutes' delay.

**Degraded mode** (`cove degraded start`) is for a GitHub that answers, but slowly or only some of the time. GitCove takes pushes and web changes as in failover, and replays the conflict-free ones in the background as GitHub answers, without waiting for plan approval. Conflicts and uncertain items still wait for you (`cove replay plan`, then skip or retry), and work for default and deploy branches still goes up as a PR. `cove degraded pause` stops the background replay but keeps taking work: an item already being sent finishes and its result is recorded, and nothing else goes to GitHub until `cove degraded start` resumes it. `cove degraded stop` refuses pushes again; whatever is left replays, and GitCove returns to normal once it's done. If everything goes down while degraded, `cove failover start` takes over. Local edits of new issues are refused in degraded mode, because replay may already be posting them.

### Issues and comments during failover

GitCove's web interface is at its root URL. Sign in with your GitCove token. It shows each repo's issues and PRs with their comments as last synced, with a banner giving the mode and the age of the copy. During failover you can comment, open an issue, add or remove a label, close or reopen, and edit a title or description, on issues and PRs alike. Everything else is read-only. A new issue gets a temporary number such as `L-3`. Mention it elsewhere as `#L-3` and replay writes in the real number. Changes show on the page as queued until replay. Bodies are shown as plain text.

**Replay** posts these after the pushes, in order: new issues, then comments, then labels and close or reopen, then edits. Something that mentions a new issue waits until that issue is on GitHub. If it never gets there (see below), what depends on it waits for an admin too, and `cove replay plan` says so.

- Everything is posted by the App, starting "Originally by `ben` at 15:12 UTC during a GitHub outage", unless the person linked their GitHub account (below).
- A label change applies just that label, never the whole set.
- Replay reads the issue's current version from GitHub before a close, reopen or edit. If GitHub still has what GitCove showed, the change is applied. If GitHub already has the new value, nothing is done. If GitHub changed it to something else, GitHub's version stays and the proposal is posted as a comment. GitHub has no conditional update for issues, so a change landing in the instant between that read and the write can still be overwritten. It stays in GitHub's edit history.
- Writes keep to a budget below GitHub's documented content limits: at least 1 second apart, at most 60 a minute and 450 an hour. On a rate limit, replay stops every request until GitHub allows them again. `cove replay plan` shows how long the queue takes at that pace.

**Nothing is posted twice.** Each post carries a hidden marker (`<!-- gitcove:op=... -->`), and the item is marked as sending before the request. If the answer is lost, or GitCove restarts, replay looks for the marker with GitHub's filtered listings before doing anything else. It waits at least 15 seconds after sending and checks three times, because those listings lag behind writes. If the marker turns up, the item is done. If not, the item becomes **uncertain**, and replay never posts it again by itself, because GitHub may simply not be listing it yet. Check GitHub, then `cove replay skip ID` if it's there, or `cove replay retry ID` to post it again. What GitHub refuses, such as a comment on a deleted or locked issue, goes to the conflicts list with the text kept.

### Pull requests, reviews and merging during failover

Each repo has a branch list (`/OWNER/REPO/branches`) with a comparison against the default branch. During failover:

- **Open a PR** from the comparison page, or with `cove pr create` inside a clone. It gets an `L-n` number. "Fixes #12" or "Fixes #L-2" in its text works as on GitHub.
- **Review** on a PR's "Files changed" page. Each line of the diff has a comment box. One submit sends the summary, the verdict (comment, approve or request changes) and every comment you wrote. The review is tied to the commit shown. Line comments can be replied to on the PR's page.
- **Merge** with the button on the PR's page or `cove pr merge 12`, once the PR's current commit has the approvals the repo needs (default 1, never the author's own; `cove repo approvals OWNER/REPO N`). A new push needs a new approval. It is always a merge commit, and it's the only way a protected branch changes during failover. If the branches conflict, nothing is merged: merge the target into your branch, push, and get it approved again. GitHub's checks aren't run or required. Issues the PR says it fixes show as closed on GitCove.
- PRs from forks can be read, commented on and reviewed, but not merged.

**Replay** never pushes a merge. The PR's branch is pushed like any other, the PR is opened on GitHub (or already exists), and a note on it says it was merged on GitCove, at which commit, who approved, and which PRs were merged before it. A person merges it on GitHub, where the usual checks apply, and that closes the linked issues. Until the note is posted, the target branch on GitCove keeps the merge; then it follows GitHub again. So **work built on a local merge has that merge commit in its history** until the PRs are merged on GitHub, and if GitHub's checks reject one, later work needs rebasing.

- A review is posted as a review on its commit, by the App with the reviewer named, or under the reviewer's own name if they linked their account.
- An approval or change request counts on GitHub only if the reviewer linked their account and the PR is still at the commit they reviewed. Otherwise the review is posted as a comment that says "Approved on GitCove at commit ..., not applied".
- If GitHub won't take the review, for example because its commit went to a `gitcove/...` fallback branch, it is posted as an ordinary comment quoting each file and line. A reply whose parent comment is gone is posted the same way.
- If the PR was closed on GitHub without merging, the merge goes to the conflicts list and the admin is alerted.
- `cove replay plan` flags a new issue or PR whose title matches one opened on GitHub since: probably re-filed by hand. Skip it unless both are wanted.

Not supported: resolving conversations, draft status, assignees, changing a PR's base, and GitHub's check results.

#### Posting under people's own names (optional)

With `COVE_APP_CLIENT_ID`, `COVE_APP_CLIENT_SECRET_FILE` and `COVE_USER_KEY_FILE` set, each developer can run `cove client authorise` while GitHub is up. Their replayed work then appears under their GitHub name, with the App's badge, and ends with the time it was written. In the App's settings, tick "Enable Device Flow" and keep "Expire user authorization tokens" on.

- GitCove runs GitHub's device flow itself, so the tokens never reach the developer's machine. `cove token list` shows which tokens are linked to which GitHub account.
- A person's token is used only for what they did on GitCove. Everyone else's work is posted by the App.
- Tokens are encrypted with the key in `COVE_USER_KEY_FILE`. GitHub replaces the refresh token on every refresh, and GitCove saves the new one before using the access token it came with. Linking, refreshing and dropping a token are written to the audit log.
- A token GitHub refuses, or one whose refresh token expired (after about six months), is dropped, and the App posts instead. Nothing waits for it.
- **The risk:** anyone who gets the server and the key can act as those people, within the App's permissions, until they revoke the App in their GitHub settings. Revoking a GitCove token also deletes its GitHub token. Leave the three settings unset to keep everything posted by the App.

### For developers

Once, while things are normal (Git needs a credential helper, such as `osxkeychain` or `libsecret`):

```sh
COVE_TOKEN=<token> cove client setup -url https://gitcove.example.com -org my-org
```

This also installs the agent: a launchd agent on macOS (`~/Library/LaunchAgents/dev.gitcove.agent.plist`, log in `~/Library/Logs/cove-agent.log`) a systemd user service on Linux (`cove-agent`), or a Task Scheduler task on Windows (`GitCoveAgent`, experimental, untested on a real machine). Every 30 seconds it asks GitCove's `/status` for the mode. In failover or degraded mode (paused or not) it sends Git for `github.com/my-org/...` (HTTPS and SSH URLs) to GitCove, so existing clones work unchanged. Otherwise it sends it back to GitHub. It acts only when the mode changes, so a switch you make by hand holds until the next one. Every switch shows a system notification (`osascript` on macOS, `notify-send` on Linux, a PowerShell toast on Windows). If it can't reach GitCove, it changes nothing.

On Windows the task doesn't restart the agent if it dies; it starts again at the next login. To remove it: `schtasks /delete /tn GitCoveAgent /f`.

Without the agent (`-agent=false`, or another OS), switch by hand:

```sh
cove client on    # Git sends github.com/my-org/... to GitCove
cove client off   # back to GitHub
```

Either way, only the `url.<gitcove>/my-org/.insteadOf` rule in your global Git config changes. Without the CLI, `git clone https://x:<token>@gitcove.example.com/my-org/api.git` works too. Any username works, and the token is the password.

If your admin enabled it, run `cove client authorise` once while GitHub is up, so your comments and issues from an outage are posted under your own GitHub name. It prints a code to enter at github.com/login/device.

### Status

`GET /status` is public. It returns the mode, how many changes are still on their way to GitHub, repo counts, and the oldest sync time (the age of the copy). For each health check path (`api`, `api-anon`, `git-https`, `git-ssh`) it gives whether the last check answered in time, the error rate over the window, and the last delay. It shows no repo names or error text. The agent reads it.

The web interface's banner shows the mode, the number of changes on their way, and the copy's age. It warns when the copy may be out of date: older than three sync intervals, or its last sync failed. In normal mode it says when GitHub isn't answering on some path.

## How the mirror works

- Each repo is a bare repository named by its GitHub ID, so a rename keeps it.
- A fetch writes only GitHub's copy of the refs, under `refs/gitcove/github/`. The refs developers see (`refs/heads`, `refs/tags`, `refs/pull`) are then moved to match in one atomic compare-and-swap update. Branches with queued or conflicted pushes are left alone. A fetch that fails or is cut off changes nothing, and the next cycle retries it.
- Each sync also lists each repo's branches to record GitHub's `protected` flag, and syncs its issues, PRs, comments and labels. Issues and comments are listed oldest update first from a saved cursor, so the first sync of a big repo resumes where it stopped. An idle repo costs two 304 responses and a label listing per cycle. Deleted comments stay on GitCove until the weekly full scan of milestone 7.
- Pushes go through a `pre-receive` hook (`cove hook pre-receive`) installed outside every repo. Clients can't write `refs/gitcove/`.
- `refs/gitcove/` is hidden from clients' ref listings. Whether a client can still fetch a hidden commit by its ID is tested in milestone 7.
- The installation token reaches `git` through the environment, never the command line.
- The health check makes an API call (`/rate_limit`, which doesn't count against the limit) and an unauthenticated Git request. Any Git answer below 500 counts as up.

## Development

```sh
go test ./...                                                        # includes the five fault campaigns on 10 seeds
COVE_SEEDS=200 go test -race ./internal/mirror ./internal/replay     # the nightly campaigns
```

`fakehub/` is a GitHub stand-in. It serves App authentication, repo and branch listing, Git smart HTTP including pushes, and PR creation. It refuses pushes the way GitHub refuses them from an App: protected branches, and workflow trees that match no branch tip. It logs every push, and it injects seeded faults: API or Git down, HTTP 500s, timeouts, pack responses cut off partway, writes carried out but never answered, slow answers, SSH down, and failing installation tokens. It also answers SSH greetings, for the health check. The campaign in `internal/mirror` runs rounds of random faults and GitHub-side changes (new commits, new and deleted branches, force-pushes). It checks every store with `git fsck` after each round, and checks that one clean cycle afterwards brings every served ref level with GitHub. To repeat a failing seed, run `COVE_SEEDS=N go test -run 'TestFaultCampaign/seed=K$' ./internal/mirror`.

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

`TestDegradedCampaign` runs the replay and PR campaigns in degraded mode. GitHub is slow and flaky instead of down (up to 40% errors, timeouts, lost responses, rate limits, delays, lagging listings), and replay runs in the background, with kills, while people push, open PRs, review and merge. The campaigns' checks then apply unchanged. `TestUnlandedPushWaits` stops a push between the hook recording it and Git moving the branch, which is where background replay could otherwise drop it. The tests in `internal/health` use a fake clock against `fakehub`'s delay, flapping, SSH-only, HTTPS-only and installation-token faults. They check the alerts and their hints, and that the checks never change the mode. `TestFailoverEndToEnd` runs the real agent: it switches on failover, holds while GitCove restarts, switches back, follows `cove degraded start`, `pause` and `stop`, and notifies on every switch.

CI runs gofmt, `go vet`, golangci-lint, the tests with `-race`, and a Docker build. A nightly run uses 200 seeds.

## Code layout

```
cmd/cove/            server, admin CLI and developer agent in one binary
internal/ghclient/   GitHub API behind one interface: App JWT, installation tokens, ETags, rate limiter, write budget, device flow
internal/gitstore/   bare repos, fetch, served refs, push, pre-receive hook, local merges, diffs, git http-backend
internal/store/      SQLite in WAL mode, migrations, mode, outbox, mirrored issues, PRs and reviews, tokens, audit log
internal/mirror/     the sync loop (the plan's internal/sync): refs, branch protection, issues and comments
internal/replay/     replay plan, history comparison, pushes and fallback PRs, replay of issues, comments, PRs, reviews and merges
internal/auth/       people's GitHub tokens: device flow, encryption, refresh
internal/health/     probes on four paths and alerts
internal/web/        HTTP: Git for developers, the web interface, /status
fakehub/             fault-injecting GitHub stand-in
```

## Licence

Apache 2.0.
