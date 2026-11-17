// Command cove is the GitCove server and its admin CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/tabaak/gitcove/internal/auth"
	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/health"
	"github.com/tabaak/gitcove/internal/mirror"
	"github.com/tabaak/gitcove/internal/replay"
	"github.com/tabaak/gitcove/internal/store"
	"github.com/tabaak/gitcove/internal/web"
)

const usage = `usage: cove <command>

  serve                                    run the server: sync, health checks, replay, Git over HTTP, web interface, /status
  sync                                     mirror every repo once and exit
  token create -name NAME -repo OWNER/REPO [-repo ...]
                                           issue a token; its secret is printed once
  token list
  token revoke ID

  failover start                           accept pushes while GitHub is down
  failover allow-protected on|off          allow direct pushes to protected branches, for this failover only
  failover end                             refuse pushes again, catch up with GitHub and show the replay plan
  degraded start                           GitHub slow or flaky: take pushes and changes, replay conflict-free ones in the background
  degraded pause                           keep taking work, but stop replaying it until degraded start
  degraded stop                            refuse pushes again; what's left replays, then GitCove is back to normal
  replay plan                              dry run: what replay would push and post, and what needs a human
  replay approve                           let the server replay the plan
  replay skip ID | replay retry ID         settle a conflict or uncertain item: drop it, or queue it again
  branch list
  branch mark|unmark OWNER/REPO BRANCH protected|deploy
                                           extra protected branches; deploy branches reach GitHub only through a PR
  repo pr-only OWNER/REPO on|off           send default and deploy branch work as PRs (default on)
  repo approvals OWNER/REPO N              approvals a PR needs before it can be merged on GitCove (default 1)
  restore OWNER/REPO [BRANCH]              list branch tips saved before GitHub rewrote or deleted them
  restore [-push] OWNER/REPO BRANCH STAMP  bring one back as the new branch gitcove/restored-BRANCH-STAMP;
                                           -push also creates it on GitHub (read-write App only)

  client setup -url URL -org ORG [-agent=false] [-notify]
                                           on a developer machine: save GitCove's address and token (COVE_TOKEN or stdin),
                                           and install the agent (launchd, systemd user service, or Task Scheduler on Windows)
  client on | client off                   send Git for the organisation to GitCove, or back to GitHub, by hand
  client agent [-every 30s]                what setup installs: switches Git to GitCove in failover and degraded mode, and back
  client authorise                         while GitHub is up: let GitCove post your outage work under your GitHub name
  pr create [-title T] [-body B] [-base BRANCH]
                                           during a failover, in a clone: open a PR on GitCove from the current branch
  pr merge NUMBER|L-n                      during a failover: merge an approved PR into its base on GitCove
  version

Configuration comes from COVE_* environment variables (see README.md).
`

// version is set by the release build (-ldflags -X main.version=...).
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cove:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "version":
		fmt.Println("cove", version)
		return nil
	case "hook": // run by git-receive-pack, not by people
		os.Exit(gitstore.RunHook(args[1:]))
	case "client":
		return clientCmd(args[1:])
	case "pr":
		return prCmd(args[1:])
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		return serve(cfg)
	case "sync":
		a, err := build(cfg)
		if err != nil {
			return err
		}
		defer a.store.Close()
		return a.mirror.Cycle(context.Background())
	case "token":
		return tokenCmd(cfg, args[1:])
	case "failover":
		return failoverCmd(cfg, args[1:])
	case "degraded":
		return degradedCmd(cfg, args[1:])
	case "replay":
		return replayCmd(cfg, args[1:])
	case "branch":
		return branchCmd(cfg, args[1:])
	case "repo":
		return repoCmd(cfg, args[1:])
	case "restore":
		return restoreCmd(cfg, args[1:])
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

type config struct {
	Data, Listen, TLSCert, TLSKey     string
	API, GitURL, Org, AppID, KeyFile  string
	ClientID, ClientSecretFile        string
	UserKeyFile                       string
	AlertWebhook                      string
	WebhookSecretFile                 string
	SyncEvery, HealthEvery, FetchTime time.Duration
	ReplayEvery                       time.Duration
	SSH                               string        // GitHub's SSH server for the health probe; "" skips it
	Retention                         time.Duration // how long history refs and tombstones are kept
}

func loadConfig() (config, error) {
	c := config{
		Data:    env("COVE_DATA", "./data"),
		Listen:  env("COVE_LISTEN", ":8080"),
		TLSCert: os.Getenv("COVE_TLS_CERT"),
		TLSKey:  os.Getenv("COVE_TLS_KEY"),
		API:     strings.TrimSuffix(env("COVE_GITHUB_API", "https://api.github.com"), "/"),
		GitURL:  env("COVE_GITHUB_GIT", "https://github.com"),
		Org:     os.Getenv("COVE_ORG"),
		AppID:   os.Getenv("COVE_APP_ID"),
		KeyFile: os.Getenv("COVE_APP_KEY_FILE"),

		ClientID:         os.Getenv("COVE_APP_CLIENT_ID"),
		ClientSecretFile: os.Getenv("COVE_APP_CLIENT_SECRET_FILE"),
		UserKeyFile:      os.Getenv("COVE_USER_KEY_FILE"),

		AlertWebhook:      os.Getenv("COVE_ALERT_WEBHOOK"),
		WebhookSecretFile: os.Getenv("COVE_WEBHOOK_SECRET_FILE"),
		SSH:               env("COVE_GITHUB_SSH", "github.com:22"),
	}
	if c.SSH == "off" {
		c.SSH = ""
	}
	days, err := strconv.Atoi(env("COVE_RETENTION_DAYS", "90"))
	if err != nil || days < 1 {
		return c, errors.New("COVE_RETENTION_DAYS: want a whole number of days, 1 or more")
	}
	c.Retention = time.Duration(days) * 24 * time.Hour
	for _, d := range []struct {
		dst  *time.Duration
		name string
		def  string
	}{
		{&c.SyncEvery, "COVE_SYNC_INTERVAL", "5m"},
		{&c.HealthEvery, "COVE_HEALTH_INTERVAL", "30s"},
		{&c.FetchTime, "COVE_FETCH_TIMEOUT", "10m"},
		{&c.ReplayEvery, "COVE_REPLAY_INTERVAL", "30s"},
	} {
		if *d.dst, err = time.ParseDuration(env(d.name, d.def)); err != nil {
			return c, fmt.Errorf("%s: %w", d.name, err)
		}
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type app struct {
	store  *store.Store
	git    *gitstore.Store
	mirror *mirror.Mirror
	replay *replay.Replayer
	users  *auth.Users
	health *health.Checker
	log    *slog.Logger
}

func openStore(cfg config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.Data, 0o700); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(cfg.Data, "cove.db"))
}

func build(cfg config) (*app, error) {
	if cfg.Org == "" || cfg.AppID == "" || cfg.KeyFile == "" {
		return nil, errors.New("COVE_ORG, COVE_APP_ID and COVE_APP_KEY_FILE are required")
	}
	pem, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	key, err := ghclient.ParseKey(pem)
	if err != nil {
		return nil, fmt.Errorf("COVE_APP_KEY_FILE: %w", err)
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, err
	}
	gs, err := gitstore.New(filepath.Join(cfg.Data, "repos"), cfg.GitURL)
	if err != nil {
		st.Close()
		return nil, err
	}
	gs.Keep = cfg.Retention
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	gh := &ghclient.App{API: cfg.API, AppID: cfg.AppID, Key: key, Org: cfg.Org, HTTP: &http.Client{Timeout: time.Minute}, Web: cfg.GitURL}
	users, err := userTokens(cfg, gh, st, log)
	if err != nil {
		st.Close()
		return nil, err
	}
	m := &mirror.Mirror{GH: gh, Store: st, Git: gs, FetchTimeout: cfg.FetchTime, Log: log, Wake: make(chan struct{}, 1)}
	return &app{
		store:  st,
		git:    gs,
		mirror: m,
		users:  users,
		replay: &replay.Replayer{GH: gh, Store: st, Git: gs, Mirror: m, Log: log, Alert: alerter(cfg.AlertWebhook, log), Users: users},
		health: &health.Checker{GH: gh, HTTP: &http.Client{Timeout: 10 * time.Second}, API: cfg.API, GitURL: gs.GitURL, SSHAddr: cfg.SSH,
			Store: st, Log: log, Alert: alerter(cfg.AlertWebhook, log)},
		log: log,
	}, nil
}

// userTokens sets up keeping people's GitHub tokens (decision D3) if all three settings are
// given, and returns nil, so the App posts everything, if none are.
func userTokens(cfg config, gh *ghclient.App, st *store.Store, log *slog.Logger) (*auth.Users, error) {
	switch n := btoi(cfg.ClientID != "") + btoi(cfg.ClientSecretFile != "") + btoi(cfg.UserKeyFile != ""); n {
	case 0:
		return nil, nil
	case 3:
	default:
		return nil, errors.New("COVE_APP_CLIENT_ID, COVE_APP_CLIENT_SECRET_FILE and COVE_USER_KEY_FILE go together: set all three, or none")
	}
	secret, err := os.ReadFile(cfg.ClientSecretFile)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(cfg.UserKeyFile)
	if err != nil {
		return nil, err
	}
	gh.ClientID, gh.ClientSecret = cfg.ClientID, strings.TrimSpace(string(secret))
	u, err := auth.New(gh, st, log, key)
	if err != nil {
		return nil, fmt.Errorf("COVE_USER_KEY_FILE: %w", err)
	}
	return u, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// alerter tells the admin: always in the log, and also by POSTing {"text": msg} to webhook if set
// (which suits Slack, Mattermost and similar incoming webhooks).
func alerter(webhook string, log *slog.Logger) func(string) {
	return func(msg string) {
		log.Error("ALERT: " + msg)
		if webhook == "" {
			return
		}
		body, _ := json.Marshal(map[string]string{"text": "GitCove: " + msg})
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(webhook, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Error("alert webhook failed", "err", err)
			return
		}
		resp.Body.Close()
	}
}

func serve(cfg config) error {
	a, err := build(cfg)
	if err != nil {
		return err
	}
	defer a.store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go a.mirror.Run(ctx, cfg.SyncEvery)
	go a.replay.Run(ctx, cfg.ReplayEvery)
	go a.health.Run(ctx, cfg.HealthEvery, 15*time.Second)

	// No write timeout: clones of large repos can take a long time.
	ws := &web.Server{Store: a.store, Git: a.git, Health: a.health, Users: a.users, Log: a.log, StaleAfter: 3 * cfg.SyncEvery, Wake: a.mirror.Wake}
	if cfg.WebhookSecretFile != "" {
		secret, err := os.ReadFile(cfg.WebhookSecretFile)
		if err != nil {
			return err
		}
		if ws.WebhookSecret = bytes.TrimSpace(secret); len(ws.WebhookSecret) == 0 {
			return errors.New("COVE_WEBHOOK_SECRET_FILE is empty")
		}
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: ws.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	a.log.Info("listening", "addr", cfg.Listen, "tls", cfg.TLSCert != "")
	if cfg.TLSCert == "" {
		a.log.Warn("serving plain HTTP: tokens travel in the clear unless a TLS proxy such as Caddy sits in front")
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type repoList []string

func (r *repoList) String() string     { return strings.Join(*r, ",") }
func (r *repoList) Set(v string) error { *r = append(*r, v); return nil }

func tokenCmd(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("token: want create, list or revoke")
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	actor := cliActor()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("token create", flag.ContinueOnError)
		name := fs.String("name", "", "who or what the token is for")
		var repos repoList
		fs.Var(&repos, "repo", "OWNER/REPO the token may read (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || len(repos) == 0 {
			return errors.New("token create: -name and at least one -repo are required")
		}
		id, secret, err := st.CreateToken(ctx, actor, *name, repos)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "token %d created; this is the only time its secret is shown:\n", id)
		fmt.Println(secret)
		return nil
	case "list":
		toks, err := st.Tokens(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tCREATED\tREVOKED\tGITHUB\tREPOS")
		for _, t := range toks {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.CreatedAt, t.RevokedAt, t.GitHub, strings.Join(t.Repos, ","))
		}
		return w.Flush()
	case "revoke":
		if len(args) != 2 {
			return errors.New("token revoke: want one token ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("token revoke: %w", err)
		}
		return st.RevokeToken(ctx, actor, id)
	}
	return fmt.Errorf("token: unknown subcommand %q", args[0])
}
