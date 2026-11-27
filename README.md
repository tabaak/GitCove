<h1><picture><source media="(prefers-color-scheme: dark)" srcset="docs/logo/kit/gitcove-horizontal-dark.svg"><img src="docs/logo/kit/gitcove-horizontal.svg" alt="GitCove" width="300"></picture></h1>

A self-hosted mirror of your GitHub organisation (or personal account) that your team can keep working on while GitHub is down. When GitHub is back, GitCove replays that work to it.

During an outage, developers push branches to GitCove. In its web interface they can open issues and PRs, comment, review and merge.

Sending that work back to GitHub (the "replay") is safe, even if GitCove crashes partway through:

- It never force-pushes.
- Work for the default branch arrives only as a PR.
- Nothing is posted twice.
- Newer work on GitHub is never overwritten.

**Status: 1.0.** Test results are in [`docs/results.md`](docs/results.md), the story behind it in [`docs/writeup.md`](docs/writeup.md).

## Limits

- **No CI or deploys.** GitCove doesn't run GitHub Actions. Outages that hit only Actions are far more common than full outages, and GitCove doesn't help with those.
- **Merges come back as PRs.** A merge made on GitCove shows up on GitHub as an open PR with a note, and someone merges it there.
- **Access is assigned by hand.** Removing someone on GitHub doesn't remove them from GitCove (planned for 1.1).
- **Not mirrored:** Git LFS, wikis, releases, discussions, projects, draft status, assignees, resolved conversations, check results, squash and rebase merges, and tags pushed during an outage.

## Security

- GitCove holds a full copy of your private code, issues and comments. Protect it like anything else that holds source code.
- The App's private key can write to every repo the App is installed on. Keep it readable only by GitCove, and never grant the App **Workflows**.
- A GitCove token works like a password. Always put TLS in front.
- Report vulnerabilities privately through the repository's security advisories.

## Install

You need a Linux server with Docker, enough disk for every repo, and owner rights on the GitHub organisation, or a personal GitHub account.

**1. Create a GitHub App**

For an organisation, go to: organisation settings → Developer settings → GitHub Apps → New GitHub App.

For a personal account, go to: your own Settings → Developer settings → GitHub Apps → New GitHub App.

Fill in:

- Name: anything, e.g. `cove-my-org`. Homepage URL: anything.
- Webhook: untick **Active**.
- Repository permissions: **Contents**, **Issues** and **Pull requests**, all read and write. Nothing else.

Create it, note the **App ID**, generate a private key (a `.pem` file downloads), then install the App on your organisation or personal account.

**2. Run GitCove**

Copy the `.pem` file to your server, then log in to the server.

Change the three example values, then paste these lines into the server's terminal:

```sh
# Your organisation name, or your username for a personal account
ORG=my-org
# The App ID (not the Client ID), found in the App's settings under General → About
APP_ID=123456
# Where the .pem file is on the server
KEY=./your-app.private-key.pem
```

Then paste this into the same terminal, without changing anything:

```sh
sudo install -o 10001 -m 600 "$KEY" /etc/gitcove-app.pem &&
sudo docker run -d --name gitcove --restart unless-stopped -p 127.0.0.1:8418:8418 \
  -v gitcove-data:/data -v /etc/gitcove-app.pem:/run/app.pem:ro \
  -e COVE_ORG="$ORG" -e COVE_APP_ID="$APP_ID" -e COVE_APP_KEY_FILE=/run/app.pem \
  ghcr.io/tabaak/gitcove:latest
```

Mirroring starts right away; `curl localhost:8418/status` shows progress. Keep `/data` on a Docker volume as above, not a network share.

No Docker? Download `cove` from the releases page, install `git` (with `git-http-backend`), and run `cove serve` with the same variables.

In the next steps, replace `my-org` with your `ORG` value.

**3. Set up HTTPS**

Developers must connect over HTTPS (TLS). One way is [Caddy](https://caddyserver.com):

```sh
caddy reverse-proxy --from gitcove.example.com --to localhost:8418
```

**4. Create a token for each developer**

This example gives `alice` access to two repos:

```sh
sudo docker exec gitcove cove token create -name alice -repo my-org/api -repo my-org/web
```

**5. Set up each developer's computer**

Each developer runs this once, with their own token:

```sh
brew install tabaak/tap/cove
COVE_TOKEN=<token> cove client setup -url https://gitcove.example.com -org my-org
```

This installs a small background agent that points Git at GitCove during an outage and back at GitHub afterwards. Existing clones keep working.

To upgrade, back up the volume, pull the new image and restart the container.

## During an outage

```sh
sudo docker exec gitcove cove failover start   # GitHub is down: developers work on GitCove
sudo docker exec gitcove cove failover end     # GitHub is back: prints what will be replayed
sudo docker exec gitcove cove replay approve   # send it to GitHub
```

Run through this once as a drill before you need it. For a GitHub that is slow rather than down, see degraded mode in the reference.

## More

- [`docs/reference.md`](docs/reference.md): configuration, every command, webhooks, and exactly how failover and replay behave
- [`docs/development.md`](docs/development.md): running the tests and how the code is laid out

GitCove is maintained by one person in their spare time. Bug reports with a way to reproduce them are welcome; fixes aren't promised on a schedule.

## Licence

Apache 2.0: see [`LICENSE`](LICENSE). Third-party licences are in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md). The Docker image includes GPL-licensed Alpine packages; each release ships their source as `gitcove-gpl-sources.tar.gz`.
