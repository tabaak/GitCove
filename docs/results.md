# GitCove 1.0: test results and measurements

Everything here was run on 16 November 2026, on one laptop (Apple M-series, 10 cores) and against one sandbox organisation on GitHub with three small private repos. How to repeat each part is at the end.

## The fault campaign

Each run is one seeded scenario against `fakehub`, the fault-injecting GitHub stand-in, followed by the checks of that campaign: nothing lost, nothing duplicated, no history overwritten, nothing newer overwritten, no false approvals, and a full sync that agrees with GitHub.

| Campaign | Runs | Passed | Outages | Kills of GitCove | What was replayed |
|---|---|---|---|---|---|
| Mirror (`TestFaultCampaign`) | 1,000 | 1,000 | | | Nothing: sync only |
| Webhooks (`TestWebhookCampaign`) | 1,000 | 1,000 | | | Nothing: sync only |
| Pushes (`TestReplayCampaign`) | 300 | 300 | 821 | 462 | 2,918 pushes, 303 fallback PRs |
| Issues and comments (`TestIssueCampaign`) | 300 | 300 | 756 | 1,295 | 4,841 queued changes |
| PRs, reviews and merges (`TestPullCampaign`) | 300 | 300 | 797 | 1,117 | 5,691 queued changes, 546 local merges |
| Degraded mode (`TestDegradedCampaign`) | 600 | 600 | 1,800 | 1,479 | 3,251 pushes, 297 fallback PRs, 6,464 queued changes, 630 local merges |
| **Total** | **4,500** | **4,500** | | | |

These are the final runs, after the fixes below. Before them, the push campaign failed 1 run of the first 199 on 1,000 seeds (bug 5), and the degraded campaign failed 2 of 600 (entry 9). A run mixes its faults by seed (500s, timeouts, lost answers, rate limits, lagging listings, cut-off packs, kills), so the table is by campaign and not by fault kind.

**Lost or duplicated items: 0** in 4,500 runs.

What the columns don't show:

- The mirror campaign ran 6,000 faulty sync rounds, and 5,597 of them failed to sync, as they should under faults. After each, the repository passed `git fsck`, and one clean cycle brought it level with GitHub.
- The webhook campaign syncs only when a webhook asks, with deliveries dropped, repeated, reordered, delayed and forged.
- The degraded campaign is the push and PR campaigns again with GitHub flaky instead of down (up to 40% errors) and replay running in the background while people work.
- The four replay campaigns ran on 300 seeds each, not 1,000: the slower ones take over a minute a run, and the first attempt at 1,000 was stopped after 199 runs. Before this they had only ever run on 30 to 40 seeds locally.

### Uncertain items

An item becomes `uncertain` when replay sent it, got no answer, and can't find its marker on GitHub after waiting. Replay then never posts it again by itself. The campaigns push this hard: up to 40% of writes are carried out but never answered, and listings lag by up to three times the wait.

| Campaign | Queued changes | Ended `uncertain` | Of those, already on GitHub |
|---|---|---|---|
| Issues and comments | 4,841 | 759 (16%) | 42 |
| PRs, reviews and merges | 5,691 | 1,244 (22%) | 170 |
| Degraded mode | 6,464 | 1,503 (23%) | 137 |
| **Total** | **16,996** | **3,506 (21%)** | **349 (10% of uncertain)** |

The rest ended done (11,839), as conflicts (86), or still waiting behind an uncertain item or a conflict (1,565).

"Already on GitHub" is the count that matters: those 349 are the items a blind retry would have posted twice. Each one instead waited for a person. The other nine in ten uncertain items had never reached GitHub, mostly because GitCove was killed between marking the item and sending it, and they cost the admin a question for nothing. None of this says how often `uncertain` happens against the real GitHub, where an answer is rarely lost. In the sandbox runs below, all 130 queued changes ended done and none uncertain, with three of them cut off mid-send by a kill.

## Bugs the tests found

| # | Found by | Bug | Fix |
|---|---|---|---|
| 1 | Sandbox run, milestone 3 | With `cove client on` in the admin's own Git config, the server's fetches from GitHub were redirected to GitCove itself | GitCove's Git commands ignore the user's Git config |
| 2 | Degraded campaign, milestone 5 | The push hook records a push before Git moves the branch. Replay running in between settled the push as "nothing to push", the branch followed GitHub again, and the push was lost | Replay leaves a push that hasn't landed for up to a minute. `TestUnlandedPushWaits` |
| 3 | Degraded campaign, milestone 5 | A reply to a queued review whose ID began with "c" was read as a reply to a GitHub comment, and failed on every pass (about 1 run in 16) | The two kinds of parent are told apart by format. `TestReplyParentFormat` |
| 4 | Webhook campaign at 200 seeds, milestone 6 | Two syncs in the same second saved a branch's old tip under the same history name, and the second sync failed | History names carry milliseconds |
| 5 | Replay campaign, seed 843 of 1,000, milestone 7 | Replay pushed a fallback branch and opened its PR, but the answer was lost. A second outage added a push GitHub would refuse, and the whole branch went to conflicts with no mention of the PR already on GitHub. Nothing was lost or posted twice, but the admin wasn't told | The conflict names the branch already sent. `TestConflictNamesSentFallback` |
| 6 | First test run of milestone 7 | Not a product bug: under load, a sync moved a branch between the push hook accepting a push and Git updating the ref. The developer gets Git's ordinary "cannot lock ref" and pushes again, but the campaign counted it as a failure | The campaign accepts that refusal |
| 7 | Following the README on a clean container, milestone 7 | **The Docker image couldn't serve Git at all.** Alpine ships `git-http-backend` in a separate package, so every clone got a 500. CI has a check for exactly this, but CI had never run, because the repo had no remote | The image installs `git-daemon` |
| 8 | Same | With `/data` on a macOS bind mount, the server didn't see what the admin CLI wrote (SQLite's write-ahead log needs shared memory), so `cove failover end` did nothing | Documented: keep `/data` on a Docker volume or a local Linux disk |
| 9 | Degraded campaign, 2 of its 600 runs, milestone 7 | Not a product bug: replay created a branch on GitHub, and the next sync fetched it while its branch listing failed. A branch GitHub has but whose protection GitCove doesn't know counts as protected, by design, so the developer's next push to it was refused until the following sync. The refusal says "is protected", which is a guess, and the campaign counted it as a failure | The campaign accepts that refusal. The message is unchanged |
| 10 | The sync-delay measurement, milestone 7 | Not a product bug: the measuring script ran for over an hour on one installation token, which lasts an hour, and hung | The script takes a new token |

Removing any one safety rule (no force-push, the already-on-GitHub check, the PR-only rule, the PR lookup, held branches, the marker check, the base check on edits) makes a campaign fail. That was checked as each rule was built.

## The run against real GitHub

`spikes/sandbox.sh` runs the real binary against `gitcove-sandbox/cove-spike`: a failover in which one person pushes a new branch, pushes to a branch that GitHub then moves, is refused on protected `main`, comments, edits an issue title that GitHub also edits, opens an issue, and opens a PR that says "Fixes #L-1"; a second person reviews it with a line comment, approves and merges it on GitCove. Replay is approved and GitCove is killed with `kill -9` as soon as the first change has gone out, then restarted.

Result: **24 of 24 checks pass.** On GitHub afterwards: `main` hasn't moved; the new branch has the developer's commit; the diverged branch still has GitHub's commit, and the outage commit is on one `gitcove/...` branch with one PR; the comment, the new issue, the review, the line comment (on the reviewed commit) and the merge note are each there once; the PR's "Fixes" names the new issue's real number; GitHub's title stands and the other is proposed in a comment; the approval is shown as text and doesn't count, because the reviewer never linked an account; and after a sync GitCove serves exactly GitHub's branches. In each run the kill caught one item mid-send, and replay finished it once after the restart.

It took three runs to get there, and both failures were worth having:

- Run 1 failed one check because of a mistake in the check itself.
- Run 2 failed four: the issue created on GitHub seconds before the sync wasn't in the mirror yet, so the comment on it got a 404. **GitHub's issue listing lags behind a create by a few seconds**, as the milestone 0 spikes measured, and a sync inside that window misses the issue. The next sync has it. So an item created seconds before a failover starts can be missing from GitCove until GitHub is back.

Not covered by this run: posting under a person's own account (the device flow needs a person at a browser), real webhook deliveries, and a real outage. GitHub was reachable throughout, and "down" was the admin's switch. The sandbox App also has the Workflows permission, which the README tells you not to grant, so GitHub's refusal of workflow changes wasn't exercised here.

## Measurements

| What | Result |
|---|---|
| First sync (backup) of the sandbox: 3 repos, 82 MB on disk, one holding 200 commits and 100 MB of incompressible files; 32 issues and PRs, 550 comments | 39 s |
| The same sync again with nothing changed | 14 s |
| Replay of 100 comments and 5 new branches | 117 s, all 100 comments on GitHub once |
| Replay of the sandbox scenario (9 changes), including a kill and restart | 23 to 26 s |
| Sync delay with polling every 5 minutes (the default): a new branch on GitHub until GitCove serves it, 40 branches | median 153 s, 95th percentile 289 s, longest 304 s |
| Sync delay with webhooks: a commit on GitHub, then a signed delivery, until GitCove serves it, 20 commits | median 2.5 s, 95th percentile 3.6 s, longest 14.2 s (the first after start) |
| API requests while polling every 5 minutes, 3 repos, 1.13 hours | 165 per repo per hour, of which 62 were answered "not modified" |
| A container on an empty volume to every repo mirrored and the first clone served (laptop, image already built) | 22 s |

Replay speed is set by the write budget, not by GitCove: at most one write a second, 60 a minute and 450 an hour, to stay under GitHub's documented limits. About 400 writes finish in minutes, and 2,000 take over four hours.

Polling delay is what the interval predicts: anywhere from nothing to one interval plus the sync itself, so half the interval in the middle. The webhook figures leave out GitHub's own delivery delay, because the deliveries were signed and sent from the test machine, and GitCove wasn't reachable from GitHub.

The API count is about 14 requests per repo per sync, for small repos with a new branch every 97 seconds. An App installation gets at least 5,000 requests an hour, so at the default interval the budget covers about 30 repos like these by polling alone, or about 48 if "not modified" answers are free, as GitHub documents for conditional requests. Beyond that, lengthen `COVE_SYNC_INTERVAL` and rely on webhooks. This wasn't measured on large or busy repos.

**Not measured:** time from a fresh VPS to a working instance. The 22 seconds above leaves out renting the server, installing Docker, creating the GitHub App by hand (step 1 of the README) and pulling the image. Nobody but the author has installed it from the README yet.

## Repeating this

```sh
COVE_SEEDS=1000 go test -v -timeout 12h -run 'TestFaultCampaign|TestWebhookCampaign' ./internal/mirror
COVE_SEEDS=300 go test -v -timeout 12h -run 'Campaign' ./internal/replay
./spikes/sandbox.sh          # needs the sandbox organisation, see spikes/README.md
./spikes/measure.sh bulk
./spikes/measure.sh delay    # about 70 minutes
```

A failing seed repeats exactly: `COVE_SEEDS=1000 go test -run 'TestReplayCampaign/seed=843$' ./internal/replay`. The sandbox runs' output is in `spikes/fixtures/sandbox/run.log` and `spikes/fixtures/measure/run.log`.
