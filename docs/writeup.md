# GitCove: working through a GitHub outage, and getting the work back

GitCove is a self-hosted mirror of a GitHub organisation. During an outage a team keeps pushing, commenting, opening PRs, reviewing and merging on it, and afterwards it replays that work to GitHub. This is what it took to make the replay safe, how it was tested, and what the tests found. The numbers are in [`results.md`](results.md).

## The problem, and why a second remote isn't enough

When GitHub is down, code is the easy part. Git is distributed, and a second remote on any server takes pushes. What stops is everything around the code: the PR nobody can open, the review nobody can leave, the issue thread where the incident is being discussed. A team with a second remote can push a hotfix branch somewhere, but can't review or merge it by its usual rules, and afterwards somebody copies the comments back by hand, or doesn't.

GitCove mirrors that surrounding work too, takes changes to it during an outage, and puts them back afterwards.

## The evidence, and its limits

I looked before building, and the evidence is mixed.

- **Demand is unproven.** Of 11 requests for a "backup forge" that I found on Hacker News, none asks for failover with replay, and nobody offers to pay. Two comments come close.
- **Few outages qualify.** Of 472 GitHub incidents from November 2024 to October 2026, 15 (3%) would justify failing over. The classification is one person's judgement.
- **It can't help with deploys.** Incidents that hit only GitHub Actions outnumbered full outages 84 to 8, and GitCove does nothing for those.
- **The gap looks real.** Of 27 tools I reviewed from their docs and code, none lets a team work on PRs during an outage and replay that work. I ran none of them.

So this is a tool for the rare bad day, built mostly because the replay problem is interesting.

## The design

GitCove is one Go binary with SQLite and bare Git repos. It reads GitHub as a GitHub App. Three decisions shape it.

**The admin switches, never the software.** An early design failed over automatically. I dropped it: a false alarm splits the team's work across two systems, which costs more than a few minutes' delay. GitCove probes GitHub on four paths and tells the admin what it sees. A small agent on each developer's machine follows the mode by toggling one `url.insteadOf` rule, so existing clones work unchanged.

**Replay keeps seven rules.** No ref on GitHub moves except by fast-forward. Every queued action ends in exactly one final state. Nothing is posted twice. Default and deploy branch work reaches GitHub only through a PR, so replay can't trigger a deploy of code that never ran CI. Nothing newer on GitHub is overwritten. Replay can stop anywhere and resume. And an approval counts on GitHub only if the reviewer authorised it, for the exact commit they reviewed.

**A merge on GitCove is a proposal.** A PR approved and merged locally arrives on GitHub as an open PR with a note saying who approved what. A person merges it there, where the real checks run.

## The crash table

GitHub's API has no idempotency keys. If GitCove posts a comment and the answer is lost, it can't ask "did request X succeed?". So each post carries a hidden marker, `<!-- gitcove:op=... -->`, and the item is marked `sending` in the database before the request goes out. After a crash or a lost answer, replay looks for the marker before doing anything else.

| Crash happens | What's true | On restart |
|---|---|---|
| Before the item is marked `sending` | Nothing was sent | Sends it |
| After `sending`, before the request | Nothing was sent | Looks for the marker. Absent after the wait: `uncertain` |
| GitHub saved it, the answer was lost | GitHub has it | Finds the marker, records it as done |
| GitHub refused it | Nothing posted | Conflict, kept for the admin |
| GitHub saved it, but its listing doesn't show it yet | Looks like the second row | Waits 15 seconds, checks three times, then `uncertain` |

The last row can't be made airtight. On a real repo, a new issue took 2.6 to 5.1 seconds to appear in the listing replay searches. So an item whose marker isn't found becomes `uncertain`, and replay never posts it again by itself. A person looks at GitHub and says skip or retry. I chose a rare question for the admin over a rare duplicate.

Git pushes are simpler, because Git is idempotent where the API isn't. A fast-forward push of commits GitHub already has does nothing, and the fallback branch name is stored before anything is sent, so a restart finds its own PR instead of opening a second.

## How it was tested

The test harness is `fakehub`, a stand-in for the parts of GitHub that GitCove uses, built from responses recorded on a real organisation. It misbehaves on command, from a seed, so any failure can be repeated: API or Git down, 500s, timeouts, pack files cut off, writes carried out but never answered, rate limits, slow answers, and listings that lag behind writes.

Six campaigns run random scenarios against it. Simulated developers push, comment, open PRs, review and merge during an outage, while the simulated GitHub changes the same branches and issues. Replay then runs under faults, with GitCove killed at each point in the crash table and GitHub failing again partway. After every run, six checks: nothing lost, nothing duplicated, no history overwritten, nothing newer overwritten, no false approvals, and a full sync that agrees with GitHub.

A fake only proves what it imitates. So the same scenario runs with the real binary against a real GitHub organisation: `spikes/sandbox.sh`.

## What the tests caught

The full list is in the results. Four are worth telling.

**A push lost between two steps.** The push hook records a push before Git moves the branch. In degraded mode replay runs in the background, and when it ran in that gap it found nothing new on the branch, settled the push as done, and the branch went back to following GitHub. The push was gone. Only the degraded campaign could find it, because it needs replay and a push at the same moment. Replay now leaves a push that hasn't landed for up to a minute.

**A conflict that left something out.** At seed 843 of 1,000, replay pushed a fallback branch and opened its PR, and the answer was lost. A second outage then added a push that GitHub would refuse, and the whole branch went to conflicts, with no mention of the PR already open on GitHub. Nothing was lost or posted twice, so the checks about data all passed. What failed was the replay campaign's own check that every PR replay opens is on record. The conflict now names the branch already sent.

**The image that couldn't serve Git.** The Docker image built, started, synced and showed its pages. It couldn't answer a single clone, because Alpine ships `git-http-backend` in a separate package. No campaign could find this: they all run the code, not the image. CI has a check for exactly this, and CI had never run, because the repo had no remote yet. I found it by following my own README on a clean container.

**GitHub's listing lag, again.** The second run against real GitHub failed four checks. An issue created seconds before a sync wasn't in the mirror, because GitHub's listing hadn't caught up. I had measured that lag in an early spike and designed replay around it, and still wrote a test that assumed it away. The next sync has the issue, so the cost is small: something created seconds before a failover can be missing from GitCove until GitHub is back.

## Results

- **Fault campaigns:** 4,500 seeded runs across six campaigns, with 4,174 simulated outages and 4,353 kills of GitCove in the replay ones. Lost or duplicated items: 0. Three runs failed before the final pass: one real bug (the conflict above) and two where the campaign rejected a refusal GitCove makes on purpose.
- **Uncertain items:** of 16,996 queued issue and PR changes, 3,506 (21%) ended `uncertain` under faults far worse than real ones. 349 of those were already on GitHub, so a blind retry would have posted them twice. The other nine in ten had never been sent, and cost the admin a question for nothing.
- **Real GitHub:** the full scenario, with a `kill -9` mid-replay, passes 24 of 24 checks on the third run. All 130 changes queued across the sandbox runs ended done, none uncertain.
- **Sync delay:** median 153 s when polling every 5 minutes, and 2.5 s with webhooks (not counting GitHub's own delivery delay).
- **Speed:** a first backup of three small repos (82 MB) takes 39 s. Replay of 100 comments and 5 branches takes 117 s, held to that by GitHub's write limits.

What this doesn't show: a real outage, real webhook deliveries, a repo of real size, or anybody but me installing it.

## What GitCove can't do

It doesn't run CI or deploys. It doesn't follow GitHub's permissions yet: an admin assigns repos to tokens by hand. It doesn't mirror LFS objects, wikis, releases or discussions. Squash and rebase merges, draft PRs, assignees and resolved conversations aren't supported during a failover. And one race remains open: GitHub has no conditional update for issues, so an edit landing on GitHub between replay's read and its write can be overwritten, though it stays in the edit history.

## What I'd do next

Permission sync from GitHub, so access doesn't need assigning by hand. `cove purge`, for removing data before the retention window ends. LFS detection, so a team learns before an outage that its large files aren't covered. And real use: my own repos and one team I know, for a few weeks, since no test finds what the first real outage will.
