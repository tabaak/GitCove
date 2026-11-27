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

You need:

- A Linux server with Docker and enough disk for every repo. Ports 80 and 443 must be reachable from the internet.
- A domain name where you can add DNS records.
- Owner rights on the GitHub organisation, or a personal GitHub account.

> **Already running Caddy, nginx or another web server on this server?** GitCove's install starts its own Caddy, which can't share ports 80 and 443 with yours. Add GitCove to your existing web server instead: step 3 shows how.

**1. Create a GitHub App**

For an organisation, go to: organisation settings → Developer settings → GitHub Apps → New GitHub App.

For a personal account, go to: your own Settings → Developer settings → GitHub Apps → New GitHub App.

Fill in:

- Name: anything, e.g. `cove-my-org`. Homepage URL: anything.
- Webhook: untick **Active**.
- Repository permissions: **Contents**, **Issues** and **Pull requests**, all read and write. Nothing else.

Create it, note the **App ID**, generate a private key (a `.pem` file downloads), then install the App on your organisation or personal account.

**2. Point a domain name at the server**

Choose the address developers will use to reach GitCove, such as `gitcove.example.com`. At the DNS provider for your domain, add an A record for that name, with your server's public IPv4 address as the value (your hosting provider's dashboard shows it). If the server also has a public IPv6 address, add an AAAA record for it as well.

Step 3 uses this name to get an HTTPS certificate automatically.

**3. Run GitCove**

Copy the `.pem` file to your server, then log in to the server.

Change the four example values, then paste these lines into the server's terminal:

```sh
# Your organisation name, or your username for a personal account
ORG=my-org
# The App ID (not the Client ID), found in the App's settings under General → About
APP_ID=123456
# Where the .pem file is on the server
KEY=./your-app.private-key.pem
# The domain name from step 2
DOMAIN=gitcove.example.com
```

Then paste this into the same terminal, without changing anything. It starts GitCove:

```sh
sudo install -o 10001 -m 600 "$KEY" /etc/gitcove-app.pem &&
sudo docker run -d --name gitcove --restart unless-stopped -p 127.0.0.1:8418:8418 \
  -v gitcove-data:/data -v /etc/gitcove-app.pem:/run/app.pem:ro \
  -e COVE_ORG="$ORG" -e COVE_APP_ID="$APP_ID" -e COVE_APP_KEY_FILE=/run/app.pem \
  ghcr.io/tabaak/gitcove:latest
```

Keep `/data` on a Docker volume as above, not a network share.

Next, set up HTTPS. **If the server already runs a web server**, add GitCove to it instead of starting Caddy:

- **Caddy**: add this block to your Caddyfile (usually `/etc/caddy/Caddyfile`), with your domain, then run `sudo systemctl reload caddy`. If your Caddy runs in Docker, `localhost` there is its own container: attach GitCove to Caddy's Docker network (`sudo docker network connect <network> gitcove`) and use `gitcove:8418` instead.

  ```
  gitcove.example.com {
  	reverse_proxy localhost:8418
  }
  ```

- **nginx**: follow the nginx section of [`docs/https.md`](docs/https.md).

Otherwise, paste this, also without changing anything. It starts [Caddy](https://caddyserver.com), which serves GitCove over HTTPS and gets and renews the certificate for your domain:

```sh
sudo docker run -d --name caddy --restart unless-stopped --network host \
  -v caddy-data:/data caddy:2 \
  caddy reverse-proxy --from "$DOMAIN" --to localhost:8418
```

Mirroring starts right away. To check that everything works, run:

```sh
curl https://$DOMAIN/status
```

It shows mirroring progress. Getting the first certificate can take a minute. If it still fails after that, `sudo docker logs caddy` shows why (or your own web server's logs). The usual causes:

- The DNS record doesn't point at the server yet. New records can take a while to show up. From your own computer, `dig +short gitcove.example.com` (with your domain) should print the server's IP.
- Ports 80 or 443 are blocked. Some hosting providers block them until you allow them in their firewall settings.
- Another web server already uses ports 80 and 443: the log says `address already in use`. Remove the container with `sudo docker rm -f caddy` and add GitCove to your web server as described above.

Caddy keeps retrying on its own, but waits longer between attempts. Once you've fixed the cause, `sudo docker restart caddy` retries right away.

To use nginx or your own certificate instead of Caddy, see [`docs/https.md`](docs/https.md).

No Docker? Download `cove` from the releases page, install `git` (with `git-http-backend`), run `cove serve` with the same `COVE_` variables as above, and set up HTTPS as in [`docs/https.md`](docs/https.md).

In the next steps, replace `my-org` with your `ORG` value and `gitcove.example.com` with your `DOMAIN` value.

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
