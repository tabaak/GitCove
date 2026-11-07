# GitCove

A self-hosted mirror of a GitHub organisation that developers can keep working on while GitHub is down, and that replays their work to GitHub afterwards. The build plan is in [`plan.md`](plan.md).

**Status: milestone 1 (foundation).** GitCove mirrors every repo its GitHub App can see and serves read-only clones and fetches to developers holding a GitCove token, including while GitHub is unreachable. Pushes, failover and replay come in milestone 2.

GitCove keeps a second copy of your private code. Protect the server and its disk the way you protect anything that holds source code.

## Run it

You need a GitHub App installed on the organisation, with **Contents: read** and **Metadata: read**, and its private key.

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
| `COVE_FETCH_TIMEOUT` | `10m` | Longest time a single repo fetch may take |
| `COVE_GITHUB_API`, `COVE_GITHUB_GIT` | `https://api.github.com`, `https://github.com` | GitHub endpoints, overridden only in tests |

### Admin CLI

The CLI works on the database in `COVE_DATA` directly, so run it on the server (inside the container, use `docker exec gitcove cove ...`).

```sh
cove sync                                                 # mirror every repo once
cove token create -name alice -repo my-org/api -repo my-org/web   # prints the token once
cove token list
cove token revoke 3
```

Tokens are stored only as SHA-256 hashes. An admin assigns each token's repos by hand, and a repo can be assigned only after its first sync. Token creation and revocation are written to the audit log.

### For developers

```sh
git clone https://x:<token>@gitcove.example.com/my-org/api.git
```

Any username works; the token is the password. A credential helper keeps it out of the remote URL.

### Status

`GET /status` is public and returns the mode, repo counts, the oldest sync time (the age of the copy), and whether GitHub's API and Git hosting answered the last probe. It shows no repo names or error text.

## How the mirror works

- Each repo is a bare repository named by its GitHub ID, so a rename keeps it.
- A fetch writes only GitHub's copy of the refs, under `refs/gitcove/github/`. The refs developers see (`refs/heads`, `refs/tags`, `refs/pull`) are then moved to match in one atomic update. A fetch that fails or is cut off changes nothing, and the next cycle retries it.
- `refs/gitcove/` is hidden from clients' ref listings. Whether a client can still fetch a hidden commit by its ID is tested in milestone 7.
- The installation token reaches `git` through the environment, never the command line.
- The health check makes an API call (`/rate_limit`, which doesn't count against the limit) and an unauthenticated Git request. Any Git answer below 500 counts as up.

## Development

```sh
go test ./...                                       # includes the fault campaign on 10 seeds
COVE_SEEDS=200 go test -race ./internal/mirror      # the nightly campaign
```

`fakehub/` is a GitHub stand-in that serves App authentication, repo listing and Git smart HTTP, and injects seeded faults: API or Git down, HTTP 500s, timeouts, and pack responses cut off partway. The campaign in `internal/mirror` runs rounds of random faults and GitHub-side changes (new commits, new and deleted branches, force-pushes). It checks every store with `git fsck` after each round, and checks that one clean cycle afterwards brings every served ref level with GitHub. To repeat a failing seed, run `COVE_SEEDS=N go test -run 'TestFaultCampaign/seed=K$' ./internal/mirror`.

CI runs gofmt, `go vet`, golangci-lint, the tests with `-race`, and a Docker build. A nightly run uses 200 seeds.

## Code layout

```
cmd/cove/            server and admin CLI in one binary
internal/ghclient/   GitHub API behind one interface: App JWT, installation tokens, ETags, rate limiter
internal/gitstore/   bare repos, fetch, served refs, git http-backend
internal/store/      SQLite in WAL mode, migrations, tokens, audit log
internal/mirror/     the sync loop (the plan's internal/sync)
internal/health/     API and Git probes
internal/web/        HTTP: Git for developers, /status
fakehub/             fault-injecting GitHub stand-in
```

## Licence

Apache 2.0.
