package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// clientCmd sets up a developer machine. It only touches the developer's global Git config:
// cove.url and cove.org, the token in Git's credential helper, and the url.insteadOf rule.
func clientCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("client: want setup, on or off")
	}
	switch args[0] {
	case "setup":
		fs := flag.NewFlagSet("client setup", flag.ContinueOnError)
		base := fs.String("url", "", "GitCove's address, e.g. https://gitcove.example.com")
		org := fs.String("org", "", "the GitHub organisation GitCove mirrors")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		u, err := url.Parse(*base)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || *org == "" {
			return errors.New("client setup: -url (https://...) and -org are required")
		}
		if u.Scheme == "http" {
			fmt.Fprintln(os.Stderr, "warning: plain HTTP sends your token in the clear")
		}
		tok := os.Getenv("COVE_TOKEN") // not a flag, so it stays out of shell history
		if tok == "" {
			fmt.Fprint(os.Stderr, "GitCove token: ")
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("client setup: read token: %w", err)
			}
			tok = strings.TrimSpace(line)
		}
		if helper, _ := gitOut("config", "--get", "credential.helper"); helper == "" {
			return errors.New("client setup: Git has no credential helper to keep the token; set one first, e.g. `git config --global credential.helper osxkeychain` (macOS) or `libsecret` (Linux)")
		}
		cred := fmt.Sprintf("protocol=%s\nhost=%s\nusername=x\npassword=%s\n\n", u.Scheme, u.Host, tok)
		for _, c := range [][]string{
			{"config", "--global", "cove.url", strings.TrimSuffix(*base, "/")},
			{"config", "--global", "cove.org", *org},
		} {
			if _, err := gitOut(c...); err != nil {
				return err
			}
		}
		cmd := exec.Command("git", "credential", "approve")
		cmd.Stdin = strings.NewReader(cred)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git credential approve: %v: %s", err, out)
		}
		fmt.Println("Set up. When failover starts, run `cove client on`; afterwards, `cove client off`.")
		return nil
	case "on", "off":
		base, err1 := gitOut("config", "--global", "--get", "cove.url")
		org, err2 := gitOut("config", "--global", "--get", "cove.org")
		if err1 != nil || err2 != nil {
			return errors.New("client: run `cove client setup` first")
		}
		key := "url." + base + "/" + org + "/.insteadOf"
		// Exit code 5 means there was nothing to unset.
		if _, err := gitOut("config", "--global", "--unset-all", key); err != nil && exitCode(err) != 5 {
			return err
		}
		if args[0] == "off" {
			fmt.Println("Git goes to GitHub again.")
			return nil
		}
		for _, from := range []string{"https://github.com/" + org + "/", "git@github.com:" + org + "/", "ssh://git@github.com/" + org + "/"} {
			if _, err := gitOut("config", "--global", "--add", key, from); err != nil {
				return err
			}
		}
		fmt.Printf("Git now sends %s's GitHub repos to %s. Existing clones work unchanged.\n", org, base)
		return nil
	}
	return fmt.Errorf("client: unknown subcommand %q", args[0])
}

func gitOut(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
