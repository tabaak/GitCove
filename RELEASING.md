# Releasing GitCove

A release is a tag. `.github/workflows/release.yml` does the rest: the multi-arch image on GHCR with its SBOM and provenance, the binaries, checksums and SBOMs through GoReleaser, and the source of the image's GPL packages.

## Before the first release

- [ ] Push the repo to `github.com/tabaak/gitcove`. Until then CI has never run: every "runs in CI" and "nightly" claim in `plan.md` has only been run locally.
- [ ] Let CI pass on `main`, including one scheduled nightly run.
- [ ] In the repo's settings, turn on private vulnerability reporting (the README points there), and after the first release make the `gitcove` package public.

## Each release

1. `go test -race ./...`, then `COVE_SEEDS=200 go test -race ./internal/mirror ./internal/replay`.
2. `./spikes/sandbox.sh` against the sandbox organisation. It must end `0 failed`.
3. If `go.mod` changed, run `release/notices.sh` and commit `THIRD_PARTY_NOTICES.md`.
4. `git tag -a v1.0.0 -m v1.0.0 && git push origin v1.0.0`.
5. When the workflow finishes, check the release against the list below, and install it on a clean machine from the README alone.

## Licence checklist

GitCove's own code is Apache 2.0. The image also ships Alpine packages under the GPL (Git, BusyBox, apk-tools and a few libraries), which must come with their source.

- [ ] `LICENSE` and `THIRD_PARTY_NOTICES.md` are in every archive and in the image under `/usr/share/licenses/gitcove/`.
- [ ] The release has `gitcove-gpl-sources.tar.gz`, built by `release/gpl-sources.sh` from the image just pushed. Its `INSTALLED` file lists the image's GPL packages, and each version in `VERSIONS` matches. If Alpine's branch has moved past the image, rebuild the image and run it again.
- [ ] The release has an SBOM for each archive, and the image carries its own (`docker buildx imagetools inspect ghcr.io/tabaak/gitcove:v1.0.0 --format '{{ json .SBOM }}'`).
- [ ] Nothing GitCove links is GPL: `THIRD_PARTY_NOTICES.md` shows only BSD, MIT and similar licences. Git is run as a separate program, never linked.

## Launch, by hand

- [ ] Record the demo, under 3 minutes. `spikes/sandbox.sh` runs the same steps in order, so it works as the script: the repo on both sides, GitHub blocked by a firewall rule, a failed `git push`, `cove client on`, a comment, a new issue, a PR that references it, a review with a line comment, an approval and a local merge, a conflicting commit on GitHub, GitCove killed mid-replay and restarted, and GitHub with everything present once.
- [ ] Publish `docs/writeup.md` and `docs/results.md`, and link the video from the README.
- [ ] Post the announcement. A draft:

> **Show HN: GitCove – keep working through a GitHub outage, then replay it back**
>
> GitCove is a self-hosted mirror of a GitHub organisation. When GitHub is down or crawling, an admin switches it on, and the team keeps pushing, commenting, opening PRs, reviewing and merging there. When GitHub is back, GitCove replays that work: no force-pushes, default-branch work only as PRs, nothing posted twice.
>
> The hard part is replaying to an API with no idempotency keys, across crashes. I tested it with a fake GitHub that injects faults (thousands of seeded runs: kills at every step, lost responses, lagging listings, repeat outages) and then against a real organisation. The numbers, the bugs the tests found, and what it can't do (CI and deploys stay blocked) are in the write-up.
>
> One Go binary, SQLite, Apache 2.0.
