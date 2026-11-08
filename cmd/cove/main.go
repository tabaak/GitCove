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

	"github.com/tabaak/gitcove/internal/ghclient"
	"github.com/tabaak/gitcove/internal/gitstore"
	"github.com/tabaak/gitcove/internal/health"
	"github.com/tabaak/gitcove/internal/mirror"
	"github.com/tabaak/gitcove/internal/replay"
	"github.com/tabaak/gitcove/internal/store"
	"github.com/tabaak/gitcove/internal/web"
)

const usage = `usage: cove <command>

  serve                                    run the server: sync, health checks, replay, Git over HTTP, /status
  sync                                     mirror every repo once and exit
  token create -name NAME -repo OWNER/REPO [-repo ...]
                                           issue a token; its secret is printed once
  token list
  token revoke ID

  failover start                           accept pushes while GitHub is down
  failover allow-protected on|off          allow direct pushes to protected branches, for this failover only
  failover end                             refuse pushes again, catch up with GitHub and show the replay plan
  replay plan                              dry run: what replay would push, and what needs a human
  replay approve                           let the server replay the plan
  replay skip ID | replay retry ID         settle a conflict: drop it, or queue it again
  branch list
  branch mark|unmark OWNER/REPO BRANCH protected|deploy
                                           extra protected branches; deploy branches reach GitHub only through a PR
  repo pr-only OWNER/REPO on|off           send default and deploy branch work as PRs (default on)

  client setup -url URL -org ORG           on a developer machine: save GitCove's address and token (COVE_TOKEN or stdin)
  client on | client off                   send Git for the organisation to GitCove, or back to GitHub

Configuration comes from COVE_* environment variables (see README.md).
`

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
	case "hook": // run by git-receive-pack, not by people
		os.Exit(gitstore.RunHook(args[1:]))
	case "client":
		return clientCmd(args[1:])
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
	case "replay":
		return replayCmd(cfg, args[1:])
	case "branch":
		return branchCmd(cfg, args[1:])
	case "repo":
		return repoCmd(cfg, args[1:])
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

type config struct {
	Data, Listen, TLSCert, TLSKey     string
	API, GitURL, Org, AppID, KeyFile  string
	AlertWebhook                      string
	SyncEvery, HealthEvery, FetchTime time.Duration
	ReplayEvery                       time.Duration
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

		AlertWebhook: os.Getenv("COVE_ALERT_WEBHOOK"),
	}
	var err error
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
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	gh := &ghclient.App{API: cfg.API, AppID: cfg.AppID, Key: key, Org: cfg.Org, HTTP: &http.Client{Timeout: time.Minute}}
	m := &mirror.Mirror{GH: gh, Store: st, Git: gs, FetchTimeout: cfg.FetchTime, Log: log}
	return &app{
		store:  st,
		git:    gs,
		mirror: m,
		replay: &replay.Replayer{GH: gh, Store: st, Git: gs, Mirror: m, Log: log, Alert: alerter(cfg.AlertWebhook, log)},
		health: &health.Checker{GH: gh, HTTP: &http.Client{Timeout: 10 * time.Second}, GitURL: gs.GitURL, Store: st, Log: log},
		log:    log,
	}, nil
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
	srv := &http.Server{Addr: cfg.Listen, Handler: (&web.Server{Store: a.store, Git: a.git, Health: a.health, Log: a.log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second}
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
		fmt.Fprintln(w, "ID\tNAME\tCREATED\tREVOKED\tREPOS")
		for _, t := range toks {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.CreatedAt, t.RevokedAt, strings.Join(t.Repos, ","))
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
