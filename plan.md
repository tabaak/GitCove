# GitCove: build plan

Project name: GitCove. CLI command: `cove` (decision D1). This plan is derived from `idea-v2.md`, and section numbers below refer to that document. Estimates assume part-time work and are rough.

## 1. Scope in one paragraph

GitCove is a self-hosted Go service that mirrors a GitHub organisation's code, issues, PRs and reviews. During a GitHub outage, or a slow period, developers push, comment, open issues and PRs, review and merge locally on GitCove. Afterwards, GitCove replays that work to GitHub with no loss, no duplicates and no overwritten history, including across crashes and repeat outages.

## 2. Order and timeline

| # | Milestone | Estimate | Depends on |
|---|---|---|---|
| 0 | Spikes and decisions | 3–5 days | none |
| 1 | Foundation | 1–1.5 weeks | 0 |
| 2 | Code failover and replay | About 2 weeks | 1 |
| 3 | Issue and comment replay | About 2.5 weeks | 2 |
| 4 | PRs, reviews and local merging | About 3 weeks | 3 |
| 5 | Seamless failover | 1.5–2 weeks | 4 |
| 6 | Proof and launch 1.0 | 1–1.5 weeks | 5 |
| 7 | Backup protections and mirror hardening (1.1) | About 2 weeks | 6 |
| 8 | Extras (1.2) | 1.5–2 weeks | 7 |

Milestones 0 to 6 take about 12 to 14 weeks, and the whole plan about 16 to 18. Milestones are strictly sequential, because each builds on the replay engine from the one before.

## 3. Rules that apply to every milestone

### Definition of done

A milestone is done when all of these hold:

- Its exit criteria (below) pass.
- Its fault scenarios run in CI on a fixed set of seeds, and a larger nightly run passes.
- New admin actions are written to the audit log.
- The README and the relevant design section are updated.
- Nothing from a later milestone's list was added to make it pass.

### Invariants replay must keep (section 8.1)

| # | Invariant |
|---|---|
| R1 | No ref on GitHub moves except by fast-forward. Never force-push |
| R2 | Every failover action ends in exactly one final state: done, skipped by an admin, in `conflicts` with both versions, or `uncertain` |
| R3 | An item is posted at most once unless a human re-queues it |
| R4 | Default and deploy branch work reaches GitHub only through a PR |
| R5 | Replay never overwrites newer content on GitHub |
| R6 | Replay can stop at any moment and resume. State lives in the database, and each state change is one transaction |
| R7 | A comment, review or approval is posted as a person only if that person authorised it, and an approval only for the exact commit they approved |

### Checks run after every fault scenario

1. Nothing lost (R2)
2. Nothing duplicated (R3)
3. No overwritten history, and default-branch work only through a PR (R1, R4)
4. Nothing newer overwritten (R5)
5. No false approvals (R7)
6. A second full sync finds no unexplained differences

Checks that don't apply yet are skipped until the milestone that introduces their feature.

### Code layout

Settled in milestone 1. Packages not built yet are listed where they will go.

```
cmd/cove/            server and CLI in one binary
internal/ghclient/   GitHub API behind one interface
internal/gitstore/   bare repos, refs, hooks, http-backend
internal/store/      SQLite, migrations
internal/mirror/     sync loop: backfill, polling, webhooks (not `sync`, which shadows the standard library)
internal/outbox/     not built: the item life-cycle lives in internal/store, and marker and ordering in internal/replay
internal/replay/     plan, comparison, writer, rate budget
internal/health/     probes and mode decisions
internal/agent/      developer-machine agent
internal/web/        server-rendered interface
internal/auth/       users' GitHub tokens: device flow, encryption, refresh; GitCove tokens live in internal/store
fakehub/             fault-injecting GitHub stand-in
```

## 4. Milestone 0: spikes and decisions

**Goal:** confirm the GitHub behaviour the design depends on, and settle the blocking decisions.

Tasks:

- [x] Create a sandbox organisation on GitHub Team (needed, not optional: rulesets and branch protection on private repos), a private repo `cove-spike`, and two private Apps from a manifest. App names are global, so they are `cove-ro-gitcove-sandbox` and `cove-rw-gitcove-sandbox`.
- [x] **Spike M:** confirmed. Comment listings showed new comments at once; the filtered issue listing lagged 2.6–5.1 s. All three writes with lost responses were created.
- [x] **Spike W:** refuted. The check is tree-based: without `workflows`, a push is accepted only if its workflow files match some current branch tip.
- [x] **Spike R:** not tripped. 521 content writes in under 16 minutes all succeeded, and no header reports the content budget. Keep the documented budget; the block and `Retry-After` path is tested only in `fakehub`.
- [x] **Spike U:** confirmed, using device flow. Tokens last 8 h and refresh tokens about 181 days, and refresh tokens rotate. Approvals count towards branch protection, and GitHub refuses an approval for any commit but the head.
- [x] **Spike I:** confirmed. Older commits are accepted and show as "Outdated"; commits not in the PR get a 422.
- [x] **Spike A:** confirmed. A read-only App sees repo and org rules through `rules/branches`.
- [x] **Spike E:** confirmed. Collaborators are readable, `refs/pull` fetches, and App pushes and PRs start CI.
- [x] Test that a replayed PR saying "Fixes `#431`" closes the issue when merged: confirmed.
- [x] Record real API responses from these spikes as `fakehub` fixtures: raw exchanges are in `spikes/fixtures/`; converting them to `fakehub`'s format is milestone 1 work.
- [x] Settle decisions D1 and D2 (section 11): GitCove / `cove`, and Apache 2.0. Nothing needs reserving: the repo and image live under the hosting account, and any website goes on `gitcove.vokh.dev`.
- [x] Write a short decision record listing every design point the spikes changed: `spikes/RESULTS.md`, and section 15 of the design.

**Exit:** each spike has a written result. Design sections affected by a failed spike are updated. If spike U fails, the plan continues with App-only attribution. *Spike U passed, so D3 is now only a question of trust.*

## 5. Milestone 1: foundation

**Goal:** a running service that mirrors code and serves it to developers, with the test harness started.

Tasks:

- [x] Go module `github.com/tabaak/gitcove`, build, lint, unit-test setup, Dockerfile, and CI for the project itself.
- [x] SQLite in WAL mode with migrations. Initial tables: `events`, `repos`, `actors`, `outbox`, `id_map`, `conflicts`, `sync_cursors`, `tokens`, `audit_log`, plus `token_repos` for each token's repo access.
- [x] GitHub client behind one interface: App JWT, installation tokens with refresh, ETag support, and a response-header-aware rate limiter.
- [x] Git store: bare repos, fetch into `refs/gitcove/github/*`, served refs kept separate, `git http-backend` over HTTPS.
- [x] GitCove tokens, hashed, issued and revoked from the admin CLI. Admin assigns each token's repo access by hand.
- [x] Config, structured logging, admin CLI skeleton, and a `/status` endpoint.
- [x] Basic health check with a Git probe and an API probe.
- [x] `fakehub` v1: app authentication, repo listing and Git smart HTTP from fixtures, plus a seeded fault hook for down, 500s and timeouts.

**Exit:** a repo can be cloned from GitCove with GitHub blocked, and with `fakehub` breaking the connection mid-fetch GitCove recovers on the next cycle. *Met: `TestCloneWhileGitHubDown` and `TestRecoversFromCutFetch`, plus a seeded fault campaign (10 seeds per CI run and 200 nightly; all 200 pass locally with `-race`). Also checked live: `cove sync` mirrored `gitcove-sandbox/cove-spike` with the read-only App, and a clone worked with GitHub unreachable.*

Notes: `fakehub` builds its repo objects from the recorded `create-repo` response. The other recorded fixtures get converted when their endpoints are added. Milestone 0 recorded no installation-token or repo-listing responses, so those follow GitHub's documented shape.

## 6. Milestone 2: code failover and replay

**Goal:** developers can push code through an outage and have it return to GitHub safely.

Tasks:

- [x] Persistent mode state machine: Normal, Failover, Reconciling, Replaying. Every transition is audited and survives a restart. *Replaying can also start from Normal, to replay items an admin re-queued.*
- [x] CLI: `failover start`, `failover end`, `client setup`, `client on`, `client off` (the `url.insteadOf` rule). *`client setup` stores the URL, the organisation and the token (through Git's credential helper). The agent install is milestone 5.*
- [x] `pre-receive` hook: reject pushes outside Failover, to protected branches, and force-pushes. The rejection says how to push to a branch and open a PR instead. *Changed: the hook also writes the outbox rows, before git moves any ref, and there is no `post-receive` hook. With `post-receive`, a kill between the ref update and the outbox write would lose a push (R2). A row for a push that then fails is harmless: replay finds nothing new on the branch. Tags and branch deletions are refused too. The rejection points to GitHub for the PR, because PRs on GitCove come in milestone 4.*
- [x] Protected branch detection per spike A. *Changed: only `protected` from the branch listing, which spike A showed is true for classic protection, repo rulesets and org rulesets. Rule types aren't fetched, because nothing uses them yet. "Unknown means protected" applies to branches GitHub has but GitCove hasn't seen listed. A new branch name is accepted, and GitHub judges it at replay (a refusal goes to conflicts). Read literally, the rule would refuse every new branch.* Admin list (`branch mark ... protected`) and emergency allow (`failover allow-protected`), cleared when the failover ends.
- [x] Served-versus-GitHub ref rules: a branch with unreplayed or conflicted work stays where developers left it, and others follow GitHub. Served refs move by compare-and-swap, so a push landing during a sync is never overwritten.
- [x] Reconciling: catch-up fetch, then build the replay plan. *`failover end` does both. The plan isn't stored: it's derived from the outbox and the repos, and derived again before each push.*
- [x] Replay plan CLI with dry run, a list of every branch to push, and admin approval (`replay plan`, `replay approve`, and `replay skip|retry` for conflicts).
- [x] Commit history comparison (section 8.6): already on GitHub, default or deploy branch, fast-forward, diverged. Fallback branch `gitcove/<branch>-<timestamp>` and PR.
- [x] PR-only replay for default and deploy branches, protected or not, with the setting to turn it off (`repo pr-only`). Protected branches are always PR-only, since GitHub would refuse the push. Workflow-file check per spike W: in the plan, compare each branch's `.github/workflows` tree with every current tip on GitHub, and send mismatches to `conflicts` with the reason.
- [x] ~~Replay PRs ask for a merge commit. If GitHub squash- or rebase-merges one, save the old served tip under `refs/gitcove/history/`, reset the served branch to GitHub's, audit it, and tell developers to rebase.~~ *Dropped (decided 8 Nov): a branch follows GitHub again as soon as its work is on GitHub (pushed, or on a fallback branch with a PR), not when the PR merges. Unmerged work then lives on `gitcove/...`, both on GitHub and on GitCove. So the merge method doesn't matter, and no PR polling or history-ref special case is needed.*
- [x] Outbox life-cycle for ref items, crash recovery, resume, and pause on a second outage. *Ref items end `done`, `conflict` or `skipped`, never `uncertain`. Whether a push landed is read from GitHub's refs. For a fallback PR, the branch name is stored before `sending`, replay looks up a PR from that branch before creating one, and GitHub refuses a second open PR with the same head and base. A pause alerts once (log, plus `COVE_ALERT_WEBHOOK` per decision D7's default) and retries each interval.*
- [x] `fakehub`: push endpoints, branch protection responses, minimal PR creation, and faults for kill points, repeat outages, divergent branches and developers pushing the same commits first. *Also refuses workflow-tree mismatches like GitHub, logs every push for the fast-forward check, and has a "carried out but never answered" fault for pushes and PR creation.*

**Exit:** with random kills and repeat outages in `fakehub`, no pushes are lost, no PRs are duplicated and GitHub refs move only by fast-forward. Default-branch work arrives only through a PR. Commits already pushed to GitHub create no fallback PR. *Met in `fakehub`: `TestReplayCampaign` (10 seeds per CI run, 200 nightly; 30 pass locally) checks all of these, plus a full sync agreeing with GitHub. `TestPushesDuringFailoverAndReplay` covers each case, and `TestFailoverEndToEnd` covers the real binary from `client on` to a replayed PR. Removing any one safety rule (no force, the already-on-GitHub check, the PR-only rule, the PR lookup, held branches) makes the tests fail. Not yet run against the sandbox organisation, and that needs the read-write App.*

## 7. Milestone 3: issue and comment replay

**Goal:** comments and new issues survive an outage and come back exactly once.

Tasks:

- [x] Metadata schema: `issues`, `pull_requests`, `comments`, `reviews`, `labels`. *Changed: `issues`, `comments` and `labels` only. GitHub's issues listing returns PRs too, so PRs are mirrored as issues (`is_pr`) with their conversation comments, and comments, labels, close and reopen, and edits already work on them. Reviews are only read and written in milestone 4, so the `pull_requests`, `reviews` and `review_threads` tables move there.*
- [x] Backfill and polling sync with saved cursors and the fixed-`since` or stable-first-page ETag approach (section 7). *Fixed `since`: issues and comments are listed oldest update first, and the cursor is saved with each page, so a backfill resumes. `since` is the cursor minus a minute, because listings lag behind writes (spike M), so an item updated just before the newest one seen can be listed after it. It changes only when the cursor does, so an idle repo costs two 304s and a label listing per cycle. Untested against GitHub. Deleted comments and issues stay until the weekly scan (milestone 7).*
- [x] Server-rendered web interface to browse repos, issues, PRs, comments and reviews, with a mode banner and the age of the copy. *Reviews move to milestone 4 with their mirror. Bodies are shown as escaped plain text, not rendered Markdown: safe without a sanitiser, and no new dependency. Sign-in is the GitCove token, kept in an HttpOnly, SameSite=Strict cookie, and cross-site posts are refused.*
- [x] Web writes: comment, new issue with a temporary ID (`L-n`), labels, close and reopen, and edits. *Edits are of title and description, as in section 8.6; comments can't be edited. A new issue's title and description are edited in place until replay starts on it. Labels are limited to the repo's synced labels, so replay never creates one.*
- [x] Metadata outbox with the hidden marker, the full life-cycle (`pending`, `sending`, `done`, `conflict`, `uncertain`), and marker lookup by filtered listing per spike M, waiting at least 15 s after `sending` and rechecking twice before an issue counts as unsent. *Changed: a `sending` text item whose marker is still missing after the wait and two rechecks goes to `uncertain`, and is never sent again by itself, also after a crash. A crash before the request went out (case 2) can't be told apart from a listing that lags (case 6), and resending in case 6 is a duplicate, which the exit criterion rules out. The cost is that a crash in that moment leaves an item for a human. The same wait applies to comments. What GitHub refuses (4xx) ends in `conflict`; a rate limit sends the item back to `pending`. Labels, state and edits carry no marker: they are decided again from GitHub's current version after a crash.*
- [x] Rate-limited writer: at least 1 second between writes, 60 per minute, 450 per rolling hour, a client-side write count, stop the queue on a block, and refresh installation tokens. *Covers fallback PR creation too. The count is in memory, so a restart forgets the last hour (a `shortcut:` comment in the code). The budget is set in code, not by environment.*
- [x] Replay ordering and dependencies (section 8.5): refs, issues, comments, labels and state, then edits. Rewrite `#L-n` references. Dependants wait on `conflict` or `uncertain` items. *A new issue never waits for another new issue: two that mention each other would wait forever, so a mention of one not yet on GitHub stays as written. Everything else waits as specified, and the plan says what for. The instance stays in Replaying while something waits, until an admin skips or retries.*
- [x] Conflict rules (section 8.6): per-change labels, GitHub's state kept on close or reopen conflicts, GitHub's text kept on edit conflicts with the proposal posted as a comment. *"GitHub also changed" is judged by value: replay reads the issue's current version, applies a change only if GitHub still has the value it was made against, does nothing if GitHub already has the new value, and otherwise posts a note. A state has two values, so a note for close or reopen appears only when the change asked for the state GitCove showed and GitHub moved away from it, and a close then reopen on GitHub during the outage counts as unchanged. GitHub has no conditional update for issues, so a change landing in the milliseconds between replay's read and its write can still be overwritten. It stays in GitHub's edit history.*
- [x] Authorship: App attribution line by default. If decision D3 is yes, `client authorise` through device flow, encrypted refresh-token storage with the rotated token saved in the same transaction as the refresh, and fallback to the App. *D3 is yes (decided 8 Nov). `cove client authorise` asks GitCove, which runs the device flow itself, so the tokens never reach the developer's machine. They are encrypted with AES-256-GCM under a key in `COVE_USER_KEY_FILE`. The refresh is an HTTP call, so "the same transaction" means the rotated token is saved straight after it, before the access token is used. A token GitHub refuses is dropped and audited, and the App posts instead. A person's token also posts their labels, closes and edits. Text posted as the person ends with the original time, since GitHub can't set it.*
- [x] Basic plan screen: counts, request estimate, and the `conflicts` and `uncertain` lists. *In `cove replay plan`: counts by kind, the number of writes and the time the budget needs, each item's poster and what it waits for, and both lists. There's no web plan page.*
- [x] `fakehub`: issue and comment endpoints with listing lag, lost responses, rate limits, and out-of-order webhooks. *Changed: no webhook faults, because webhooks come in milestone 7 and sync is polling only (section 12 updated). Also added: labels, the App's identity, the device flow and user tokens with rotating refresh tokens, lost responses for every API write, and a shared number sequence for issues and PRs.*

**Exit:** a fault campaign on comments and issues passes all applicable checks, including a created item that doesn't show up in listings for a while. Case 6 in section 8.3 ends in `uncertain`, never a repost. *Met in `fakehub`: `TestIssueCampaign` (10 seeds per CI run, 200 nightly; 200 pass locally with `-race`) has two people, one with an authorised GitHub account, change issues while GitHub changes the same ones, then replays under 500s, timeouts, lost responses, rate limits, listings lagging up to three times the marker wait, kills and repeat outages. It checks R2, R3, R5 and R7 and that a full sync agrees (check 3 is about refs and doesn't apply). `TestCase6Uncertain` covers case 6 directly. Making replay resend after a restart, or apply edits without the base check, makes the campaign fail. `TestFailoverEndToEnd` covers the real binary from `client authorise` to an issue posted under the person's name. Not yet run against the sandbox organisation.*

## 8. Milestone 4: PRs, reviews and local merging

**Goal:** the full workflow in section 3 of the design works, including reviews that reach GitHub under the reviewer's name.

Tasks:

- [ ] Create PRs on GitCove from the web interface and CLI, with `L-` references and "Fixes" keywords rewritten on replay.
- [ ] PR metadata on GitCove and in replay: edits, close and reopen, draft or ready, labels, assignees and base branch, using the conflict rules in section 8.6. *Edits, close and reopen, and labels already work on PRs through the issues API since milestone 3.*
- [ ] Reviews: summary, verdict, and line comments stored with commit, path, line and side. Replies to line comments and resolved conversations stored on `review_threads`. *Also the `pull_requests`, `reviews` and `review_threads` mirror and showing reviews in the interface, moved from milestone 3.*
- [ ] PR diff view: what the PR's branch changes against its base (files, then a per-file diff), which reviews and line comments need. A read-only branch list per repo (name, last commit, "compare with" the default branch) leads to it. *Added after the 11 Nov sandbox test: the web interface had no way to see branches. No general branch switcher or file browser: developers have the code locally; add one only if people ask after a real outage.*
- [ ] Local approvals with a per-repo count (decision D4) and approver other than the author. Local merge into the target branch, as a merge commit only (decision D5). A local merge is the only way to change a protected branch.
- [ ] Local merge conflicts: refuse cleanly, leave nothing half-merged, and reset approvals when the head commit changes.
- [ ] Linked issues: close them locally when a PR with a closing keyword is merged, without replaying that close as its own item. Milestone 0 confirmed GitHub closes them when the replayed PR is merged.
- [ ] Two-state display for a PR merged locally: "Merged on GitCove" and "Open on GitHub". A PR closed on GitHub without merging goes to `conflicts`.
- [ ] Show GitHub's last known check results read-only on mirrored PRs. Local merges don't require checks.
- [ ] Fork PRs: read, comment and review only.
- [ ] Interface labels "approved on GitCove, not checked by GitHub" until replay.
- [ ] Per-PR replay in merge order, with the order listed in each PR's text. Bundle PR fallback behind a setting, to be chosen after the prototype.
- [ ] Review replay (section 8.13): line comments anchored by commit, ordinary-comment fallback when the commit is missing, approvals only for the exact commit and only for authorised reviewers (R7), otherwise a note. Replies wait behind their parent comment, with a standalone-comment fallback if it was deleted. A conversation is resolved only if no new comments arrived on GitHub in that thread. First check how resolving works through the API, which milestone 0 didn't cover.
- [ ] Likely-duplicate detection by title, at plan time and again just before each write.
- [ ] Full plan screen: branches, items, request estimate, duplicates, `conflicts`, `uncertain`, and why dependants wait.
- [ ] `fakehub`: PR, review and approval endpoints, `commit_id` handling, review threads and replies, resolved conversations, stale heads, merge conflicts, a PR closed without merging, and faults for expired tokens and a moving `main`.

**Exit:** a fault campaign including local merges, merge conflicts, reviews, replies, resolved conversations and stale approvals passes all six checks, and a smaller run passes against the sandbox organisation.

## 9. Milestone 5: seamless failover

**Goal:** switching happens without manual steps when the admin opts in, and slow GitHub is handled.

Tasks:

- [ ] Health probes on separate paths (Git over HTTPS and SSH, API reads and writes, signed-in and anonymous) with windowed error rate and delay. The alert suggests switching between SSH and HTTPS when only one path is broken.
- [ ] Agent for developer machines, installed by `cove client setup` as a systemd or launchd service. It polls GitCove's status and toggles `url.insteadOf`. If GitCove is unreachable, it changes nothing.
- [ ] Optional automatic failover with a set duration, a minimum run time and a cooldown (decision D6), and an alert channel (decision D7).
- [ ] Degraded mode with write routing, background replay within the rate budget, and optional auto-replay for items without conflicts. Exit conditions: GitHub healthy for a set time and the queue empty.
- [ ] Status banner and stale-data warnings in the interface.
- [ ] `fakehub`: delay and flapping faults, and the agent losing contact mid-switch.

**Exit:** in `fakehub`, with delay and flapping faults, the agent switches correctly and no false alarm splits the team's work. Replay in degraded mode passes all six checks.

## 10. Milestones 6 to 8

### Milestone 6: proof and launch 1.0

- [ ] Run the large fault campaign across the full scenario matrix (section 12), triage every failure, and keep a bug list with the fix for each.
- [ ] Run the full scenario against the sandbox organisation.
- [ ] Collect the measurements: runs by fault kind and any lost or duplicated items, how often case 6 gave `uncertain` and how many were real duplicates, sync delay (median and 95th percentile), API calls per repo per hour, backup and replay time at a stated repo size, and time from a fresh VPS to a working instance.
- [ ] README: install, setup, limits (including CI and deploys staying blocked, and the 84 to 8 count), security warnings about private code and user tokens, and maintenance status.
- [ ] Release: GoReleaser, multi-arch image on GHCR, GPLv2 source checklist, third-party notices and an SBOM.
- [ ] Write-up, demo video (under 3 minutes, steps in section 11 of the design), and the release announcement.

**Exit:** a stranger can install it from the README alone, and the results are published.

### Milestone 7: backup protections and mirror hardening

- [ ] History refs under `refs/gitcove/history/<timestamp>/...`, saved before any fetch that moves or removes a ref, with a daily 90-day removal job.
- [ ] Hide history and GitHub refs with `hideRefs`, and test whether a client can still fetch them by commit ID. If it can, move history into a separate repository.
- [ ] `cove restore` to `gitcove/restored-<branch>-<timestamp>`, restored branches exempt from following GitHub, and `--push` with a read-write App.
- [ ] `cove purge`, and `orphaned` repos with an admin alert.
- [ ] Permission sync from GitHub through the collaborators endpoint, which spike E showed works with Metadata: read.
- [ ] LFS detection and warnings.
- [ ] Webhooks with HMAC verification and durable de-duplication, the weekly full scan, tombstones, and ETag polling.
- [ ] A read-only App option.

**Exit:** a force-pushed branch can be brought back and survives the next sync, clients can't fetch hidden history by commit ID, and with dropped, repeated and out-of-order webhooks a second full sync finds no unexplained differences.

### Milestone 8: extras

- [ ] Design note for the isolated test runner first: no network, no secrets, one team-chosen command, result shown as information, never a deploy.
- [ ] Implement the runner and show its result on replay PRs.
- [ ] Interface polish.

**Exit:** the runner produces a result for a sample repo with no network and no secrets.

## 11. Decisions needed

| # | Decision | Needed by | Default if undecided |
|---|---|---|---|
| D1 | Name: GitCove; CLI command: `cove` | Decided | GitCove / `cove` |
| D2 | Licence: Apache 2.0 | Decided | Apache 2.0 |
| D3 | Whether to store users' refresh tokens so reviews appear under their names | Decided (8 Nov): yes, opt-in per person | No, App-only. Section 8.12 and the real approvals in 8.13 are dropped |
| D4 | Default approvals needed for a local merge | Start of milestone 4 | 1, and not the author |
| D5 | Whether local merges may also be squash or rebase. They would break commit-bound approvals | Start of milestone 4 | No. Merge commits only |
| D6 | Automatic failover defaults (duration, cooldown, degraded thresholds) | Start of milestone 5 | 10 minutes, with other values set by testing |
| D7 | Alert channel for the admin | Default taken in milestone 2 | Log plus an optional webhook (`COVE_ALERT_WEBHOOK`) |

## 12. Fault scenarios by milestone

| Milestone | Faults added to `fakehub` |
|---|---|
| 1 | GitHub fully down, partly down (API up and Git down, or the reverse), HTTP 500s, timeouts |
| 2 | Kill GitCove at random points, repeat outages, force-pushes and deleted branches on GitHub, developers pushing the same commits first, divergent branches, branch protection refusing a push |
| 3 | Rate-limit responses, a created item missing from listings for a while, lost responses, the same item edited on both sides. *Webhook faults moved to milestone 7 with webhooks* |
| 4 | Local merges while `main` moves, merge conflicts on a local merge, expired or revoked user tokens, approvals for a commit that changed, line comments on a commit that never reached GitHub, a parent comment deleted before a reply replays, a conversation resolved or given new comments on GitHub during replay, a PR closed on GitHub without merging |
| 5 | Sustained error rates of 1 to 40%, installation-token failures, SSH-only and HTTPS-only failures, GitHub flapping, the agent losing contact mid-switch |
| 7 | A repo deleted on GitHub, the App uninstalled, a sync straight after a restore, dropped, repeated and out-of-order webhooks |

Every fault is seeded, so any failing run can be repeated exactly. Proposed targets: at least 200 seeds per scenario family at each milestone exit, and thousands in milestone 6. These numbers are a starting point.

## 13. Engineering risks

| Risk | Response |
|---|---|
| Replay takes longer than estimated | Replay milestones come first. Cut from the end of the plan, never from replay |
| Case 6: GitHub's listing lags behind a successful post | Spike M measured up to 5.1 s. Wait at least 15 s, recheck twice, then `uncertain` |
| GitHub refuses pushes whose workflow files match no current branch | Spike W showed the check is tree-based. A plan check against every current tip, `conflicts`, and commits kept on GitCove |
| `fakehub` differs from real GitHub | Build it from recorded responses, and run each milestone's smaller scenario against the sandbox organisation |
| Replay triggers a deployment | PR-only rule for default and deploy branches, and the plan lists every branch pushed |
| A local merge is rejected by GitHub later | Label it clearly, alert the admin, and document the rebase |
| User tokens are stolen from the server | Opt-in, encrypted with a key outside the database, rotated and logged, and App-only mode stays fully working |
| An approval is replayed wrongly | R7, an exact-commit check, and a label until GitHub has seen it |
| Automatic failover has a false alarm or flaps | Opt-in, both Git and API must fail, a cooldown, and flapping faults in tests |
| GitHub limits or suspends API access | Stay inside documented limits, and stop the queue on any block |
| The interface keeps growing | Only features that help during an outage get built |
| GitCove falls behind GitHub's API | Alert on persistent sync errors, and state the maintenance status in the README |

## 14. If time runs short

Cut in this order:

1. Milestone 8
2. LFS detection and orphaned repos
3. Plan screen detail
4. The bundle PR fallback
5. Automatic failover, keeping the agent
6. Degraded mode

Never cut the replay invariants, the fault test, or issue and comment replay.

## 15. Stopping points

| After | What works |
|---|---|
| Milestone 2 | Code survives an outage and returns to GitHub safely |
| Milestone 3 | Comments and new issues also survive and return |
| Milestone 4 | The full workflow, with PRs, reviews and local merging |
| Milestone 5 | Switching is seamless, and slow GitHub is handled |
| Milestone 6 | Launched, with measured results |
| Milestone 7 | 90-day protection against deletions and force-pushes on GitHub |
