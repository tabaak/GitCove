# Milestone 0 spikes

Throwaway scripts that test the GitHub behaviour the design depends on. Each run saves the raw requests, response headers and bodies under `fixtures/<spike>/`, plus a `run.log`. Milestone 1 turns these into `fakehub` fixtures. Write what you find in `RESULTS.md`.

Needs `bash`, `curl`, `jq`, `openssl`, `git` and `uuidgen` (all present on macOS).

## Setup, about 15 minutes

1. **Sandbox org** on GitHub Team (one seat). Free orgs have rulesets and branch protection only on public repos, and public repos can't prove permission behaviour. Spike A's org ruleset and spike U's approval check both need it.
2. **Admin token:** a classic PAT with `repo`, `workflow` and `admin:org`, expiring in 7 days. Create `spikes/.secrets/env`:
   ```
   ORG=<sandbox org>
   ADMIN=<classic PAT>
   ```
3. `./setup.sh` creates the private repo `cove-spike`.
4. Open `register.html` in a browser and create the `ro` App, then run `./convert.sh ro <code>` and install the App on `cove-spike` only. Repeat for `rw`.
5. For spike U only: in cove-rw's App settings, tick **Enable Device Flow** and leave **Expire user authorization tokens** on.

`.secrets/` and `work/` are git-ignored. Never commit them.

## Order

Run `./a.sh`, `./w.sh`, `./m.sh`, `./i.sh`, `./fixes.sh`, `./e.sh`, `./u.sh`, `./r.sh`. The order matters:

- `fixes.sh` merges a PR into `main`, so it runs before `u.sh` protects `main`.
- `w.sh` stops halfway: give cove-rw **Workflows: write**, accept the change as owner, then run `./w.sh after`.
- `r.sh` posts up to 520 comments to find GitHub's block point. Mute notifications on the repo first, and run it last, because it risks a temporary block on cove-rw.

Each script is meant to run once on the sandbox. To rerun one cleanly, delete and recreate `cove-spike`.
