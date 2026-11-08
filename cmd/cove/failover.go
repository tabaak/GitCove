package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/tabaak/gitcove/internal/replay"
	"github.com/tabaak/gitcove/internal/store"
)

func cliActor() string { return "cli:" + env("USER", "unknown") }

func onOff(args []string, cmd string) (bool, error) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return false, fmt.Errorf("%s: want on or off", cmd)
	}
	return args[0] == "on", nil
}

func failoverCmd(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("failover: want start, end or allow-protected")
	}
	ctx := context.Background()
	switch args[0] {
	case "start":
		st, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetMode(ctx, cliActor(), store.Failover); err != nil {
			return err
		}
		fmt.Println("Failover started: GitCove accepts pushes. Developers switch with `cove client on`.")
		return nil
	case "end":
		a, err := build(cfg)
		if err != nil {
			return err
		}
		defer a.store.Close()
		if err := a.store.SetMode(ctx, cliActor(), store.Reconciling); err != nil {
			return err
		}
		fmt.Println("Failover ended: pushes are refused again. Developers switch back with `cove client off`.")
		return printPlan(ctx, a)
	case "allow-protected":
		on, err := onOff(args[1:], "failover allow-protected")
		if err != nil {
			return err
		}
		st, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.AllowProtected(ctx, cliActor(), on); err != nil {
			return err
		}
		if on {
			fmt.Println("Direct pushes to protected branches are allowed until this failover ends. Reviews are not enforced meanwhile.")
		}
		return nil
	}
	return fmt.Errorf("failover: unknown subcommand %q", args[0])
}

func replayCmd(cfg config, args []string) error {
	if len(args) == 0 {
		return errors.New("replay: want plan, approve, skip or retry")
	}
	ctx := context.Background()
	switch args[0] {
	case "plan":
		a, err := build(cfg)
		if err != nil {
			return err
		}
		defer a.store.Close()
		return printPlan(ctx, a)
	case "approve":
		st, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetMode(ctx, cliActor(), store.Replaying); err != nil {
			return err
		}
		fmt.Println("Replay approved: the server replays now, re-checking GitHub before each push.")
		return nil
	case "skip", "retry":
		if len(args) != 2 {
			return fmt.Errorf("replay %s: want one item ID", args[0])
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("replay %s: %w", args[0], err)
		}
		st, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.Resolve(ctx, cliActor(), id, args[0] == "retry"); err != nil {
			return err
		}
		if args[0] == "retry" {
			fmt.Println("Re-queued. It replays on the next `cove replay approve`.")
		}
		return nil
	}
	return fmt.Errorf("replay: unknown subcommand %q", args[0])
}

// printPlan is the dry run: it fetches the repos with queued pushes and shows what replay would do.
func printPlan(ctx context.Context, a *app) error {
	mode, err := a.store.Mode(ctx)
	if err != nil {
		return err
	}
	actions, fetchErrs, err := a.replay.Plan(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Mode: %s\n\n", mode)
	for _, e := range fetchErrs {
		fmt.Printf("warning: %v\n  (planned from the last copy; replay fetches again before pushing)\n", e)
	}
	if len(actions) == 0 {
		fmt.Println("Nothing to replay.")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "REPO\tBRANCH\tCOMMIT\tPUSHES\tACTION\tWHY")
		var pushed []string
		for _, act := range actions {
			what := string(act.Kind)
			switch act.Kind {
			case replay.FastForward:
				what = "push to " + act.Target
			case replay.PR:
				what = "push " + act.Target + ", open PR into " + act.Branch
			}
			if act.Kind == replay.FastForward || act.Kind == replay.PR {
				pushed = append(pushed, act.Repo.FullName()+":"+act.Target)
			}
			fmt.Fprintf(w, "%s\t%s\t%.8s\t%d\t%s\t%s\n", act.Repo.FullName(), act.Branch, act.Tip, len(act.Items), what, act.Reason)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(pushed) > 0 {
			fmt.Printf("\nBranches this replay pushes to GitHub. Each push can start workflows there:\n  %s\n", strings.Join(pushed, "\n  "))
		}
		fmt.Println("\nGitHub is checked again before each push, so an action can still become a PR or a conflict, never the reverse.")
	}
	held, err := a.store.RefItems(ctx, 0, "conflict", "uncertain")
	if err != nil {
		return err
	}
	if len(held) > 0 {
		fmt.Println("\nNeeds a human (`cove replay skip ID` or `cove replay retry ID`); these branches keep their GitCove commits:")
		for _, it := range held {
			fmt.Printf("  #%d %s %s, %s by %s: %s\n", it.ID, it.Repo, it.Branch, it.State, it.Pusher, it.Result)
		}
	}
	if mode == store.Reconciling {
		fmt.Println("\nApprove with: cove replay approve")
	}
	return nil
}

func repoArg(ctx context.Context, st *store.Store, full string) (store.Repo, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok {
		return store.Repo{}, fmt.Errorf("want OWNER/REPO, got %q", full)
	}
	r, err := st.RepoByName(ctx, owner, name)
	if errors.Is(err, store.ErrNotFound) {
		return r, fmt.Errorf("unknown repo %q (repos appear after the first sync)", full)
	}
	return r, err
}

func branchCmd(cfg config, args []string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch {
	case len(args) == 1 && args[0] == "list":
		marks, err := st.BranchMarks(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "REPO\tBRANCH\tMARK")
		for _, m := range marks {
			fmt.Fprintf(w, "%s\t%s\t%s\n", m.Repo, m.Branch, m.Kind)
		}
		return w.Flush()
	case len(args) == 4 && (args[0] == "mark" || args[0] == "unmark") && (args[3] == "protected" || args[3] == "deploy"):
		r, err := repoArg(ctx, st, args[1])
		if err != nil {
			return err
		}
		return st.MarkBranch(ctx, cliActor(), r, args[2], args[3], args[0] == "mark")
	}
	return errors.New("branch: want list, or mark|unmark OWNER/REPO BRANCH protected|deploy")
}

func repoCmd(cfg config, args []string) error {
	if len(args) != 3 || args[0] != "pr-only" {
		return errors.New("repo: want pr-only OWNER/REPO on|off")
	}
	on, err := onOff(args[2:], "repo pr-only")
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	r, err := repoArg(ctx, st, args[1])
	if err != nil {
		return err
	}
	return st.SetPROnly(ctx, cliActor(), r, on)
}
