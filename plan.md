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
| 6 | Backup protections and sync hardening | About 1.5 weeks | 5 |
| 7 | Proof and launch 1.0 | 1–1.5 weeks | 6 |
| 8 | 1.1 | 0.5–1 week | 7 |
| 9 | Extras (1.2) | 1.5–2 weeks | 8 |

Milestones 0 to 7 take about 13.5 to 15.5 weeks, and the whole plan about 16 to 18. Milestones are strictly sequential, because each builds on the replay engine from the one before. Backup protections and sync hardening come before launch, so the measurements and fault campaign in milestone 7 run on the final sync design.

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
- [x] Backfill and polling sync with saved cursors and the fixed-`since` or stable-first-page ETag approach (section 7). *Fixed `since`: issues and comments are listed oldest update first, and the cursor is saved with each page, so a backfill resumes. `since` is the cursor minus a minute, because listings lag behind writes (spike M), so an item updated just before the newest one seen can be listed after it. It changes only when the cursor does, so an idle repo costs two 304s and a label listing per cycle. Untested against GitHub. Deleted comments and issues stay until the weekly scan (milestone 6).*
- [x] Server-rendered web interface to browse repos, issues, PRs, comments and reviews, with a mode banner and the age of the copy. *Reviews move to milestone 4 with their mirror. Bodies are shown as escaped plain text, not rendered Markdown: safe without a sanitiser, and no new dependency. Sign-in is the GitCove token, kept in an HttpOnly, SameSite=Strict cookie, and cross-site posts are refused.*
- [x] Web writes: comment, new issue with a temporary ID (`L-n`), labels, close and reopen, and edits. *Edits are of title and description, as in section 8.6; comments can't be edited. A new issue's title and description are edited in place until replay starts on it. Labels are limited to the repo's synced labels, so replay never creates one.*
- [x] Metadata outbox with the hidden marker, the full life-cycle (`pending`, `sending`, `done`, `conflict`, `uncertain`), and marker lookup by filtered listing per spike M, waiting at least 15 s after `sending` and rechecking twice before an issue counts as unsent. *Changed: a `sending` text item whose marker is still missing after the wait and two rechecks goes to `uncertain`, and is never sent again by itself, also after a crash. A crash before the request went out (case 2) can't be told apart from a listing that lags (case 6), and resending in case 6 is a duplicate, which the exit criterion rules out. The cost is that a crash in that moment leaves an item for a human. The same wait applies to comments. What GitHub refuses (4xx) ends in `conflict`; a rate limit sends the item back to `pending`. Labels, state and edits carry no marker: they are decided again from GitHub's current version after a crash.*
- [x] Rate-limited writer: at least 1 second between writes, 60 per minute, 450 per rolling hour, a client-side write count, stop the queue on a block, and refresh installation tokens. *Covers fallback PR creation too. The count is in memory, so a restart forgets the last hour (a `shortcut:` comment in the code). The budget is set in code, not by environment.*
- [x] Replay ordering and dependencies (section 8.5): refs, issues, comments, labels and state, then edits. Rewrite `#L-n` references. Dependants wait on `conflict` or `uncertain` items. *A new issue never waits for another new issue: two that mention each other would wait forever, so a mention of one not yet on GitHub stays as written. Everything else waits as specified, and the plan says what for. The instance stays in Replaying while something waits, until an admin skips or retries.*
- [x] Conflict rules (section 8.6): per-change labels, GitHub's state kept on close or reopen conflicts, GitHub's text kept on edit conflicts with the proposal posted as a comment. *"GitHub also changed" is judged by value: replay reads the issue's current version, applies a change only if GitHub still has the value it was made against, does nothing if GitHub already has the new value, and otherwise posts a note. A state has two values, so a note for close or reopen appears only when the change asked for the state GitCove showed and GitHub moved away from it, and a close then reopen on GitHub during the outage counts as unchanged. GitHub has no conditional update for issues, so a change landing in the milliseconds between replay's read and its write can still be overwritten. It stays in GitHub's edit history.*
- [x] Authorship: App attribution line by default. If decision D3 is yes, `client authorise` through device flow, encrypted refresh-token storage with the rotated token saved in the same transaction as the refresh, and fallback to the App. *D3 is yes (decided 8 Nov). `cove client authorise` asks GitCove, which runs the device flow itself, so the tokens never reach the developer's machine. They are encrypted with AES-256-GCM under a key in `COVE_USER_KEY_FILE`. The refresh is an HTTP call, so "the same transaction" means the rotated token is saved straight after it, before the access token is used. A token GitHub refuses is dropped and audited, and the App posts instead. A person's token also posts their labels, closes and edits. Text posted as the person ends with the original time, since GitHub can't set it.*
- [x] Basic plan screen: counts, request estimate, and the `conflicts` and `uncertain` lists. *In `cove replay plan`: counts by kind, the number of writes and the time the budget needs, each item's poster and what it waits for, and both lists. There's no web plan page.*
- [x] `fakehub`: issue and comment endpoints with listing lag, lost responses, rate limits, and out-of-order webhooks. *Changed: no webhook faults, because webhooks come in milestone 6 and sync is polling only (section 12 updated). Also added: labels, the App's identity, the device flow and user tokens with rotating refresh tokens, lost responses for every API write, and a shared number sequence for issues and PRs.*

**Exit:** a fault campaign on comments and issues passes all applicable checks, including a created item that doesn't show up in listings for a while. Case 6 in section 8.3 ends in `uncertain`, never a repost. *Met in `fakehub`: `TestIssueCampaign` (10 seeds per CI run, 200 nightly; 200 pass locally with `-race`) has two people, one with an authorised GitHub account, change issues while GitHub changes the same ones, then replays under 500s, timeouts, lost responses, rate limits, listings lagging up to three times the marker wait, kills and repeat outages. It checks R2, R3, R5 and R7 and that a full sync agrees (check 3 is about refs and doesn't apply). `TestCase6Uncertain` covers case 6 directly. Making replay resend after a restart, or apply edits without the base check, makes the campaign fail. `TestFailoverEndToEnd` covers the real binary from `client authorise` to an issue posted under the person's name. Not yet run against the sandbox organisation.*

## 8. Milestone 4: PRs, reviews and local merging

**Goal:** the full workflow in section 3 of the design works, including reviews that reach GitHub under the reviewer's name.

Tasks:

- [x] Create PRs on GitCove from the web interface and CLI, with `L-` references and "Fixes" keywords rewritten on replay. *A new PR is a new-issue item with a head and base, so it shares the `L-n` numbers, the marker lookup and the reference rewriting. The web form is on the branch comparison page; `cove pr create` posts the same form from inside a clone. Replay opens it once its branch is on GitHub, from the `gitcove/...` fallback branch if the push went there, and waits while that push needs a human.*
- [x] PR metadata on GitCove and in replay. *Cut down: edits, close and reopen, and labels, which work through the issues API since milestone 3. **Dropped: draft or ready, assignees and base branch.** GitHub's REST API can't change draft status (GraphQL only), assignees need a sync of who can be assigned, and none of the three is something a team needs during a two-hour outage. Do them on GitHub afterwards.*
- [x] Reviews: summary, verdict, and line comments stored with commit, path, line and side, and replies to line comments. The `pull_requests`, `reviews` and `review_comments` mirror, shown in the interface. *A reply names the first comment of its thread, so there is no `review_threads` table. Reviews have no repo-wide listing, so they are read per PR, only for open PRs that changed since the last sync. **Dropped: resolving conversations.** It exists only in the GraphQL API, for reading and writing, milestone 0 never tested it, and it would mean a second API client and a GraphQL stand-in in `fakehub` for a tick mark people can set on GitHub after the outage.*
- [x] PR diff view, and a read-only branch list per repo that leads to it. *`/OWNER/REPO/branches`, `/compare?head=BRANCH`, and "Files changed" on a PR. The diff is the review form: each line has a comment box, and one submit sends the verdict, the summary and every box that was filled in. No JavaScript. Diffs over 5,000 lines are cut off.*
- [x] Local approvals with a per-repo count (decision D4: 1, set with `cove repo approvals`) and approver other than the author. Local merge into the target branch, as a merge commit only (decision D5). A local merge is the only way to change a protected branch. *An approval counts only for the PR's current commit, so a new push resets it without any bookkeeping. Approvals GitHub already had for that commit count too. No fast-forward merges: always a merge commit, as GitHub's button makes.*
- [x] Local merge conflicts: refuse cleanly, leave nothing half-merged, and reset approvals when the head commit changes. *The merge is computed with `git merge-tree` in the bare repo, and the branch moves by compare-and-swap, so there is nothing to half-merge. The merge is recorded before the branch moves and taken back if the move fails; replay checks the branch really contains it.*
- [x] Linked issues: close them locally when a PR with a closing keyword is merged, without replaying that close as its own item.
- [x] Two-state display for a PR merged locally. A PR closed on GitHub without merging goes to `conflicts`. *Changed: checked when the merge is replayed, with an alert. After replay GitHub is the only truth again, and someone closing the PR there later is their decision, not a conflict, so GitCove doesn't keep watching.*
- [ ] ~~Show GitHub's last known check results read-only on mirrored PRs.~~ *Dropped: the App has neither the Checks nor the Commit statuses permission, so this means asking every installation for more access, plus a request per open PR per sync. The merge button says checks aren't run or required. Revisit with the interface polish in milestone 9 if people ask.*
- [x] Fork PRs: read, comment and review only.
- [x] Interface labels "approved on GitCove, not checked by GitHub" until replay.
- [x] Per-PR replay in merge order, with the order listed in each PR's text. *Changed: the merge commit is never pushed. The branch a locally merged PR came from is pushed like any other, the PR is opened or already exists, and a note on it says it was merged on GitCove, at which commit, approved by whom, and which PRs were merged before it. The target branch on GitCove keeps the merge until that note is posted, then follows GitHub again. **Dropped: the bundle PR fallback.** The chain wasn't awkward in the prototype, and it was already fourth on the cut list.*
- [x] Review replay (section 8.13). *Posted as a review on the commit it was made on. An approval or change request is applied only with the reviewer's own token and only while that commit is the PR's head on GitHub (R7); otherwise the review only comments and states the verdict in words. If GitHub refuses the review (the commit isn't part of the PR, or the PR moved under an approval), it goes up as an ordinary comment quoting each file and line. A reply waits for its parent's review, and becomes an ordinary comment quoting the parent if the parent is gone. Resolving is dropped, see above.*
- [x] Likely-duplicate detection by title. *Changed: at plan time only. `cove replay plan` flags a new issue or PR whose title matches one opened on GitHub after it was queued. **Dropped: the check before each write.** It could only send the item to conflicts, and "retry" would then need a way to say "post it anyway", for what the design already calls a human check.*
- [x] Full plan screen. *Still `cove replay plan`: it now counts new PRs, reviews, replies and local merges, and lists duplicates. No web page; "plan screen detail" is third on the cut list.*
- [x] `fakehub`: PR, review and approval endpoints, `commit_id` handling, replies, stale heads, merge conflicts, a PR closed without merging, and faults for revoked tokens and a moving `main`. *No resolved conversations, as above.*

**Exit:** a fault campaign including local merges, merge conflicts, reviews, replies, resolved conversations and stale approvals passes all six checks, and a smaller run passes against the sandbox organisation. *Met in `fakehub`, without resolved conversations: `TestPullCampaign` (10 seeds per CI run, 200 nightly; 40 pass locally) has three people, one with an authorised GitHub account, push, open PRs, review with line comments, reply, approve and merge locally, while on GitHub PR branches and `main` move, PRs are closed or merged, line comments are deleted and the user token is revoked. It checks R1 to R4 and R7 and that a full sync agrees. `TestPullRequestFlow` covers each case through the web interface, including a refused conflicting merge and a stale approval. Not yet run against the sandbox organisation. `cove pr create` and `cove pr merge` are untested beyond compiling: they post the same forms the tests post.*

## 9. Milestone 5: seamless failover

**Goal:** switching happens without manual steps when the admin opts in, and slow GitHub is handled. *Changed: the admin switches every mode by hand; the agent makes it seamless for developers.*

Tasks:

- [x] Health probes on separate paths (Git over HTTPS and SSH, API reads and writes, signed-in and anonymous) with windowed error rate and delay. The alert suggests switching between SSH and HTTPS when only one path is broken. *Changed: four paths, all probed at once: the API signed in and anonymous (`rate_limit`, which doesn't count against the 60-an-hour anonymous limit), Git over HTTPS, and Git over SSH (the server's greeting, so no key is needed). **Dropped: write probes.** A probe that writes leaves something on GitHub every 30 seconds. Replay's own writes already show whether writing works, and replay pauses and alerts when they fail. Signed-in Git isn't probed separately: it's what every sync fetch does, and failed syncs already show on `/status` and in the banner. An answer over 5 s counts as an error. Alerts go out when the set of paths that haven't answered for 2 minutes changes. A path that's fully down counts towards the alert, not the error rate, so a server that can't reach port 22 isn't permanently "flaky" (`COVE_GITHUB_SSH=off` silences it).*
- [x] Agent for developer machines, installed by `cove client setup` as a systemd or launchd service. It polls GitCove's status and toggles `url.insteadOf`. If GitCove is unreachable, it changes nothing. *`cove client agent`, polling `/status` every 30 s: the rule is on in failover and degraded mode, paused or not. With `cove client setup -notify`, every switch shows a system notification (`osascript`, `notify-send` or a PowerShell toast); it's off by default, since developers hear of switches from the team message and the line `git push` prints. It acts only when that answer changes, so `cove client on` or `off` by hand holds until the next switch. `-agent=false` skips the service. The launchd and systemd install has never been run: tests pass `-agent=false` so they don't install a service on the test machine. The agent itself is tested in the end-to-end test.*
- [x] ~~Optional automatic failover with a set duration, a minimum run time and a cooldown (decision D6)~~, and an alert channel (decision D7). ***Dropped: automatic failover.** A false switch splits the team's work, so only the admin starts and ends failover. The alert channel stays: alerts name the paths down and suggest `cove failover start` when every path is. It also carries one team message per `failover start` and `end` and `degraded start`, `pause` and `stop`, saying what changed and what developers should expect (checked in `TestFailoverEndToEnd`). An accepted push prints one `remote:` line saying when it reaches GitHub: when it's back in failover, shortly in degraded mode, once replay resumes while paused, and "as a PR" where replay will open one (`TestAcceptedPushMessage`).*
- [x] Degraded mode with write routing, background replay within the rate budget, and optional auto-replay for items without conflicts. Exit conditions: GitHub healthy for a set time and the queue empty. ***Changed: manual only.** `cove degraded start`, `pause` and `stop`; no automatic entry or exit. The 20%-of-5-minutes and 5 s thresholds only trigger an alert suggesting `cove degraded start`, and 10 healthy minutes one suggesting `cove degraded stop`. `pause` (mode `degraded-paused`) keeps taking work but stops replay: an item mid-send finishes and records its result, then replay starts no other (`TestPauseMidSend`); `start` resumes it. Paused, the healthy alert suggests both `start` and `stop`. `stop` refuses pushes and goes to normal, or to replaying if items are left, which returns to normal by itself. **No separate auto-replay switch:** in degraded mode replay posts conflict-free items without approval; conflicts and uncertain items wait for the admin, and default-branch work still goes up as a PR. "Write routing" is the agent turning the rule on, with a system notification on every switch if set up with `-notify`. **Fixed a race this exposed:** the hook records a push before git moves the branch, and with replay running meanwhile, a push caught in between was settled as "nothing to push". The branch then followed GitHub, and the push was lost. Replay now leaves a push that hasn't landed for up to a minute. Local edits of new issues are refused in degraded mode, because replay may already have read them.*
- [x] Status banner and stale-data warnings in the interface. *The banner shows degraded mode, the changes still on their way, and paths GitHub isn't answering on. It warns when the copy is older than three sync intervals or its last sync failed. `/status` gives each path's state, error rate and delay, and the count of changes on their way.*
- [x] `fakehub`: delay and flapping faults, and the agent losing contact mid-switch. *Delay, SSH down and installation-token faults, and an SSH greeting to probe. Flapping is the tests switching faults between checks. Losing contact is the end-to-end test killing the server mid-failover. `Commit` became a compare-and-swap, so GitHub-side commits in the degraded campaign can't undo a push landing at the same moment.*

**Exit:** in `fakehub`, with delay and flapping faults, the agent switches correctly and no false alarm splits the team's work. Replay in degraded mode passes all six checks. *Met in `fakehub`: no switch happens without the admin, so no false alarm can split the team's work. The tests in `internal/health` drive the checker with a fake clock through 40 minutes of flapping, slow answers, 40% errors, a failing installation token, SSH-only or HTTPS-only outages and a full outage, and check the alerts and their hints, and that the mode never changes. Removing the "down paths aren't flaky" rule fails them. `TestFailoverEndToEnd` runs the real agent: it switches on failover, changes nothing while GitCove is killed and restarted, switches back, follows `cove degraded start`, `pause` and `stop`, and notifies once per switch. `TestDegradedCampaign` (10 seeds per CI run, 200 nightly; 40 pass locally with `-race`) runs the push and PR campaigns in degraded mode, with replay in the background while people work, and passes their checks (all six). `TestUnlandedPushWaits` fails without the race fix. The campaign also found a milestone 4 bug: a reply to a queued review whose op ID started with "c" was read as a reply to a GitHub comment and failed every pass (about 1 in 16). It's fixed, with `TestReplyParentFormat`. Not yet run against the sandbox organisation, and the agent's launchd and systemd install has never been run.*

## 10. Milestones 6 to 9

### Milestone 6: backup protections and sync hardening

- [x] History refs under `refs/gitcove/history/<timestamp>/...`, saved before any fetch that moves or removes a ref, with a daily 90-day removal job. *Changed: saved when the **served** ref (`refs/heads`, `tags`, `pull`) is about to lose commits, in the same atomic `update-ref` that moves it, so a crash can't move a ref without saving it. Until then the old tip is still reachable from the served ref, so nothing can collect it in between (the fetch also runs with `--no-auto-gc`). A fast-forward saves nothing, and `refs/pull/N/merge` is skipped, because GitHub rebuilds it whenever the base moves. **No separate daily job:** that same update deletes history refs older than the window, since every sync already reads all refs. The window is `COVE_RETENTION_DAYS` (default 90).*
- [x] Hide history and GitHub refs with `hideRefs`, and test whether a client can still fetch them by commit ID. If it can, move history into a separate repository. *Changed: **it could, and a separate repository wouldn't have fixed it.** Over protocol v2 (Git's default) `upload-pack` serves any object in the repo by ID, with or without a ref. A force-pushed commit stays in the served repo's object store until garbage collection, so moving the history ref elsewhere changes nothing. Under v0 and v1, a stateless HTTP `upload-pack` serves only what's reachable from advertised refs. So GitCove drops the `Git-Protocol` header and serves v0 only, and history stays in the same repo. `TestRestoreForcePushedBranch` checks v0, v1 and v2 through the client, and also sends v0 and v2 `want` requests by hand, as a hostile client would; it fails without the fix. Costs and limits: every fetch gets the full ref list, including all `refs/pull/*`. During failover, a token holder who knows a hidden commit's ID could craft a push building on it (receive-pack's connectivity check finds the object present). Both are in the README.*
- [x] `cove restore` to `gitcove/restored-<branch>-<timestamp>`, restored branches exempt from following GitHub, and `--push` with a read-write App. *`cove restore OWNER/REPO [BRANCH]` lists saved tips; with a stamp it creates the branch (compare-and-swap against "doesn't exist", so it never moves one), and syncs neither move nor delete `gitcove/restored-*`. `-push` pushes without force; a refusal says to fetch and push by hand, as with the read-only App. Restores and pushes go in the audit log. `TestRestoreEndToEnd` runs the real binary, including `-push`.*
- [x] Webhooks with HMAC verification and durable de-duplication, the weekly full scan, tombstones, and ETag polling. *`POST /webhook`, on when `COVE_WEBHOOK_SECRET_FILE` is set: it checks `X-Hub-Signature-256` and reads only `repository.id` and `action`. A delivery is a hint to sync that repo, the same code path polling uses, so no payload reaches the database. The delivery ID and the repo's `sync_wanted` flag are written in one transaction before the 2xx (IDs kept a week; GitHub redelivers for 3 days). The flag is cleared as the repo's sync starts, so a delivery during a sync asks for another. The mirror loop wakes on a delivery and syncs the flagged repos between full cycles. **Added: a delivery with `action: deleted` makes that sync a full scan**, since only a full scan sees deletions. The full scan runs weekly per repo (not on its first sync, which just backfilled). It lists every issue, comment and line comment, checks each item missing from the listing with GitHub (the issue, or the issue's or PR's own comment listing; a listing paged while items change can skip one), and moves the gone ones to `tombstones` as JSON. Transferred issues stay, because GitHub redirects to them. **ETag polling was already done in milestone 3.***

**Exit:** a force-pushed branch can be brought back and survives the next sync, clients can't fetch hidden history by commit ID, and with dropped, repeated and out-of-order webhooks a second full sync finds no unexplained differences. *Met in `fakehub`. `TestRestoreForcePushedBranch` restores a force-pushed branch, syncs straight after, then again once history has expired, and the branch stays; no protocol fetches hidden history by ID. The mirror fault campaign now also checks that every tip GitCove ever served is still in a served or history ref; skipping the save fails it. `TestWebhookCampaign` (10 seeds per CI run, 200 nightly) makes commits, force-pushes, deleted branches, and new, edited and deleted issues and comments, announced by webhooks that are dropped, repeated, reordered, redelivered late or forged. It syncs only on webhooks. A repo whose webhooks got through must match GitHub, except items deleted while every "deleted" webhook was dropped, which are the only explained differences. Then one full sync must match GitHub exactly, and a second must change nothing. Breaking de-duplication, or the deleted-means-full-scan rule, fails it. The 200-seed run found one bug: two webhook syncs within a second both saved `feature` under the same second-resolution stamp, and the second sync failed. Stamps now have milliseconds. `TestFullScanBuries` covers line comments too. `TestWebhookEndToEnd` sends a signed delivery to the real `cove serve` with syncs an hour apart, and the commit arrives. Not yet run against the sandbox organisation; webhook signatures follow GitHub's documentation and haven't been checked against a real delivery.*

### Milestone 7: proof and launch 1.0

- [ ] Run the large fault campaign across the full scenario matrix (section 12), triage every failure, and keep a bug list with the fix for each.
- [ ] Run the full scenario against the sandbox organisation.
- [ ] Collect the measurements: runs by fault kind and any lost or duplicated items, how often case 6 gave `uncertain` and how many were real duplicates, sync delay (median and 95th percentile), API calls per repo per hour, backup and replay time at a stated repo size, and time from a fresh VPS to a working instance.
- [ ] README: install, setup, limits (including CI and deploys staying blocked, and the 84 to 8 count), security warnings about private code and user tokens, and maintenance status.
- [ ] Release: GoReleaser, multi-arch image on GHCR, GPLv2 source checklist, third-party notices and an SBOM.
- [ ] Write-up, demo video (under 3 minutes, steps in section 11 of the design), and the release announcement.

**Exit:** a stranger can install it from the README alone, and the results are published.

### Milestone 8: 1.1

- [ ] LFS detection and warnings.
- [ ] `cove purge`, and `orphaned` repos with an admin alert.
- [ ] Permission sync from GitHub through the collaborators endpoint, which spike E showed works with Metadata: read.
- [ ] A read-only App option.

**Exit:** a repo deleted on GitHub or left by an uninstalled App becomes `orphaned` with an alert and `cove purge` removes it, an LFS repo shows a warning, a collaborator removed on GitHub loses access on GitCove at the next sync, and an instance runs with only the read-only App.

### Milestone 9: extras

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
| D4 | Default approvals needed for a local merge | Default taken in milestone 4 | 1, and not the author. Per repo: `cove repo approvals` |
| D5 | Whether local merges may also be squash or rebase. They would break commit-bound approvals | Default taken in milestone 4 | No. Merge commits only |
| D6 | Alert thresholds (automatic failover was dropped in milestone 5) | Default taken in milestone 5 | A path down after 2 minutes; "slow or flaky" at 20% of 5 minutes' checks failing or over 5 s; healthy again after 10 minutes. Hints only: the admin switches. All guesses, not yet set by testing against GitHub |
| D7 | Alert channel for the admin | Default taken in milestone 2 | Log plus an optional webhook (`COVE_ALERT_WEBHOOK`), which also carries the team's mode-switch messages |

## 12. Fault scenarios by milestone

| Milestone | Faults added to `fakehub` |
|---|---|
| 1 | GitHub fully down, partly down (API up and Git down, or the reverse), HTTP 500s, timeouts |
| 2 | Kill GitCove at random points, repeat outages, force-pushes and deleted branches on GitHub, developers pushing the same commits first, divergent branches, branch protection refusing a push |
| 3 | Rate-limit responses, a created item missing from listings for a while, lost responses, the same item edited on both sides. *Webhook faults moved to milestone 6 with webhooks* |
| 4 | Local merges while `main` moves, merge conflicts on a local merge, expired or revoked user tokens, approvals for a commit that changed, line comments on a commit that never reached GitHub, a parent comment deleted before a reply replays, a conversation resolved or given new comments on GitHub during replay, a PR closed on GitHub without merging |
| 5 | Sustained error rates of 1 to 40%, installation-token failures, SSH-only and HTTPS-only failures, GitHub flapping, the agent losing contact mid-switch |
| 6 | A sync straight after a restore, dropped, repeated and out-of-order webhooks |
| 8 | A repo deleted on GitHub, the App uninstalled |

Every fault is seeded, so any failing run can be repeated exactly. Proposed targets: at least 200 seeds per scenario family at each milestone exit, and thousands in milestone 7. These numbers are a starting point.

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

1. Milestone 9
2. Milestone 8
3. Plan screen detail
4. The bundle PR fallback
5. ~~Automatic failover, keeping the agent~~ (dropped in milestone 5)
6. Degraded mode

Never cut the replay invariants, the fault test, or issue and comment replay.

## 15. Stopping points

| After | What works |
|---|---|
| Milestone 2 | Code survives an outage and returns to GitHub safely |
| Milestone 3 | Comments and new issues also survive and return |
| Milestone 4 | The full workflow, with PRs, reviews and local merging |
| Milestone 5 | Switching is seamless, and slow GitHub is handled |
| Milestone 6 | 90-day protection against deletions and force-pushes on GitHub, and webhook sync that stays correct |
| Milestone 7 | Launched, with measured results |
| Milestone 8 | LFS warnings, orphaned repos and purge, permissions from GitHub, and a read-only App option |
