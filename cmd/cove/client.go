package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/tabaak/gitcove/internal/store"
)

// clientCmd sets up a developer machine. It only touches the developer's global Git config:
// cove.url, cove.org and cove.notify, the token in Git's credential helper, and the url.insteadOf rule; and
// the agent's service file.
func clientCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("client: want setup, on, off, agent or authorise")
	}
	switch args[0] {
	case "setup":
		fs := flag.NewFlagSet("client setup", flag.ContinueOnError)
		base := fs.String("url", "", "GitCove's address, e.g. https://gitcove.example.com")
		org := fs.String("org", "", "the GitHub organisation GitCove mirrors")
		withAgent := fs.Bool("agent", true, "install the agent, which switches Git to GitCove and back by itself")
		notifyOn := fs.Bool("notify", false, "have the agent show a system notification when it switches Git")
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
			{"config", "--global", "--bool", "cove.notify", strconv.FormatBool(*notifyOn)},
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
		if !*withAgent {
			fmt.Println("Set up. When failover starts, run `cove client on`; afterwards, `cove client off`.")
			return nil
		}
		if err := installAgent(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the agent isn't installed (%v).\nRun `cove client agent` at login yourself, or `cove client on` and `cove client off` by hand.\n", err)
			return nil
		}
		fmt.Println("Set up. The agent switches Git to GitCove when failover starts and back when it ends.")
		return nil
	case "on", "off":
		base, org, err := clientConfig()
		if err != nil {
			return err
		}
		if err := setRule(base, org, args[0] == "on"); err != nil {
			return err
		}
		if args[0] == "off" {
			fmt.Println("Git goes to GitHub again.")
			return nil
		}
		fmt.Printf("Git now sends %s's GitHub repos to %s. Existing clones work unchanged.\n", org, base)
		return nil
	case "agent":
		fs := flag.NewFlagSet("client agent", flag.ContinueOnError)
		every := fs.Duration("every", 30*time.Second, "how often to ask GitCove for its mode")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return agent(*every)
	case "authorise", "authorize":
		return authorise()
	}
	return fmt.Errorf("client: unknown subcommand %q", args[0])
}

func clientConfig() (base, org string, err error) {
	base, err1 := gitOut("config", "--global", "--get", "cove.url")
	org, err2 := gitOut("config", "--global", "--get", "cove.org")
	if err1 != nil || err2 != nil {
		return "", "", errors.New("client: run `cove client setup` first")
	}
	return base, org, nil
}

// setRule turns on or off the url.insteadOf rule that sends Git for org's GitHub repos to GitCove.
func setRule(base, org string, on bool) error {
	key := "url." + base + "/" + org + "/.insteadOf"
	// Exit code 5 means there was nothing to unset.
	if _, err := gitOut("config", "--global", "--unset-all", key); err != nil && exitCode(err) != 5 {
		return err
	}
	if !on {
		return nil
	}
	for _, from := range []string{"https://github.com/" + org + "/", "git@github.com:" + org + "/", "ssh://git@github.com/" + org + "/"} {
		if _, err := gitOut("config", "--global", "--add", key, from); err != nil {
			return err
		}
	}
	return nil
}

// agent asks GitCove for its mode every interval, and turns the rule on in the modes that take
// writes (failover and degraded, paused or not) and off otherwise, with a system notification if
// setup was given -notify (the team hears of switches from GitCove's channel and pushes). It
// acts only when that answer changes, so `cove client on` or `off` by hand holds until the next
// switch. If GitCove can't be reached it changes nothing: a developer keeps working where they
// were rather than being split from the team by a network blip. Only one runs per user: a second
// copy exits at once, successfully, so two can't fight over the rule.
func agent(every time.Duration) error {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(cfg, "gitcove"), 0o700); err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(cfg, "gitcove", "agent.lock"))
	if errors.Is(err, errLocked) {
		log.Print("another cove client agent is already running; this one exits")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close() // keeps the lock held, and the file alive, while the agent runs
	base, org, err := clientConfig()
	if err != nil {
		return err
	}
	notifyOn, _ := gitOut("config", "--global", "--bool", "--get", "cove.notify")
	hc := &http.Client{Timeout: 10 * time.Second}
	applied, unreachable := "", false
	for ; ; time.Sleep(every) {
		mode, err := remoteMode(hc, base)
		if err != nil {
			if !unreachable {
				log.Printf("can't reach GitCove, changing nothing: %v", err)
			}
			unreachable = true
			continue
		}
		unreachable = false
		want := map[bool]string{true: "on", false: "off"}[store.Mode(mode).TakesWrites()]
		if want == applied {
			continue
		}
		if err := setRule(base, org, want == "on"); err != nil {
			log.Printf("switching Git %s: %v", want, err)
			continue
		}
		msg := fmt.Sprintf("GitCove is in %s mode: Git for %s goes to %s", mode, org, map[string]string{"on": base, "off": "GitHub"}[want])
		log.Print(msg)
		if notifyOn == "true" && (applied != "" || want == "on") { // not on every login while GitHub is fine
			notify(msg)
		}
		applied = want
	}
}

// notify shows msg as a system notification, best effort.
func notify(msg string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("osascript", "-e", "on run argv", "-e", `display notification (item 1 of argv) with title "GitCove"`, "-e", "end run", msg)
	case "linux":
		c = exec.Command("notify-send", "GitCove", msg)
	case "windows":
		// A toast under PowerShell's own app ID, which every Windows registers; msg goes in through
		// the environment so it is never parsed as script or XML.
		c = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", `$m = [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
$x = $m::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$t = $x.GetElementsByTagName('text')
$null = $t[0].AppendChild($x.CreateTextNode('GitCove'))
$null = $t[1].AppendChild($x.CreateTextNode($env:COVE_NOTE))
$m::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show([Windows.UI.Notifications.ToastNotification]::new($x))`)
		c.Env = append(os.Environ(), "COVE_NOTE="+msg)
	default:
		return
	}
	if out, err := c.CombinedOutput(); err != nil {
		log.Printf("notification: %v %s", err, out)
	}
}

var errLocked = errors.New("locked by another process")

func remoteMode(hc *http.Client, base string) (string, error) {
	resp, err := hc.Get(base + "/status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var st struct{ Mode string }
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || resp.StatusCode != http.StatusOK || st.Mode == "" {
		return "", fmt.Errorf("/status: %s %v", resp.Status, err)
	}
	return st.Mode, nil
}

// installAgent runs `cove client agent` at login: a launchd agent on macOS, a systemd user
// service on Linux, a Task Scheduler task on Windows.
func installAgent() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var path, body string
	var cmds [][]string
	switch runtime.GOOS {
	case "darwin":
		path = filepath.Join(home, "Library", "LaunchAgents", "dev.gitcove.agent.plist")
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.gitcove.agent</string>
  <key>ProgramArguments</key><array><string>%s</string><string>client</string><string>agent</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, html.EscapeString(exe), html.EscapeString(filepath.Join(home, "Library", "Logs", "cove-agent.log")))
		domain := "gui/" + strconv.Itoa(os.Getuid())
		cmds = [][]string{{"launchctl", "bootout", domain + "/dev.gitcove.agent"}, {"launchctl", "bootstrap", domain, path}}
	case "linux":
		cfg, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		path = filepath.Join(cfg, "systemd", "user", "cove-agent.service")
		body = fmt.Sprintf("[Unit]\nDescription=GitCove agent: sends Git to GitCove during GitHub outages\n\n"+
			"[Service]\nExecStart=%q client agent\nRestart=always\n\n[Install]\nWantedBy=default.target\n", exe)
		cmds = [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "cove-agent"},
			{"systemctl", "--user", "restart", "cove-agent"}}
	case "windows":
		u, err := user.Current()
		if err != nil {
			return err
		}
		f, err := os.CreateTemp("", "cove-agent-*.xml")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		_, err = f.Write(utf16LE(taskXML(exe, u.Username)))
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
		cmds = [][]string{{"schtasks", "/end", "/tn", "GitCoveAgent"},
			{"schtasks", "/create", "/f", "/tn", "GitCoveAgent", "/xml", f.Name()},
			{"schtasks", "/run", "/tn", "GitCoveAgent"}}
	default:
		return fmt.Errorf("no service manager known for %s", runtime.GOOS)
	}
	if path != "" { // Windows keeps the task in Task Scheduler, not in a file
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	for i, c := range cmds {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil && (runtime.GOOS == "linux" || i != 0) { // bootout and /end fail when it wasn't running yet
			return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	return nil
}

// taskXML is the agent's Task Scheduler task: at account's logon only, as account without admin
// rights, on battery too, with no time limit. The trigger repeats every minute and a running copy
// makes Task Scheduler skip it, so an agent that dies is back within a minute (RestartOnFailure
// only covers a task that fails to start). conhost --headless gives the agent a console with no
// window, so there is nothing to see or close, and git and PowerShell inherit it.
func taskXML(exe, account string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <UserId>%[2]s</UserId>
      <Repetition><Interval>PT1M</Interval></Repetition>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%[2]s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>conhost.exe</Command>
      <Arguments>--headless "%[1]s" client agent</Arguments>
    </Exec>
  </Actions>
</Task>
`, html.EscapeString(exe), html.EscapeString(account))
}

// utf16LE encodes s as UTF-16 with a byte-order mark, as schtasks /xml expects.
func utf16LE(s string) []byte {
	b := []byte{0xFF, 0xFE}
	for _, r := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, r)
	}
	return b
}

// coveRequest makes a POST to GitCove with the developer's token from Git's credential helper.
func coveRequest(path string, form url.Values) (*http.Response, error) {
	base, err := gitOut("config", "--global", "--get", "cove.url")
	if err != nil {
		return nil, errors.New("client: run `cove client setup` first")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "credential", "fill")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("protocol=%s\nhost=%s\n\n", u.Scheme, u.Host))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("no GitCove token in Git's credential helper (run `cove client setup`): %w", err)
	}
	var tok string
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "password="); ok {
			tok = v
		}
	}
	req, err := http.NewRequest("POST", base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("x", tok)
	// No timeout: authorising waits while the code is entered. Redirects are the caller's to read.
	return (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
}

// authorise asks GitCove to link this token to the developer's GitHub account through GitHub's
// device flow, and prints what GitCove streams back: the code to enter, then the outcome.
func authorise() error {
	resp, err := coveRequest("/api/authorise", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitCove said: %s", strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	last := ""
	for sc.Scan() {
		last = sc.Text()
		fmt.Println(last)
	}
	if !strings.HasPrefix(last, "ok:") {
		return errors.New("not authorised")
	}
	return sc.Err()
}

var originRe = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// prCmd opens or merges a PR on GitCove from inside a clone, during a failover. It posts the
// same forms as the web interface.
func prCmd(args []string) error {
	// The configured URL, not `git remote get-url`, which would apply the insteadOf rule.
	origin, err := gitOut("config", "--get", "remote.origin.url")
	m := originRe.FindStringSubmatch(origin)
	if err != nil || m == nil {
		return errors.New("pr: run this inside a clone whose origin is the repo on GitHub")
	}
	repo := "/" + m[1] + "/" + m[2]
	var path string
	form := url.Values{}
	switch {
	case len(args) >= 1 && args[0] == "create":
		fs := flag.NewFlagSet("pr create", flag.ContinueOnError)
		title := fs.String("title", "", "the PR's title (default: the last commit's subject)")
		body := fs.String("body", "", "the PR's description")
		base := fs.String("base", "", "the branch to merge into (default: the repo's default branch)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		head, err := gitOut("symbolic-ref", "--short", "HEAD")
		if err != nil {
			return errors.New("pr create: check out the branch to open the PR from")
		}
		if *title == "" {
			*title, _ = gitOut("log", "-1", "--format=%s")
		}
		path, form = repo+"/pulls", url.Values{"title": {*title}, "body": {*body}, "base": {*base}, "head": {head}}
	case len(args) == 2 && args[0] == "merge":
		path = repo + "/issues/" + url.PathEscape(strings.TrimPrefix(args[1], "#")) + "/merge"
	default:
		return errors.New("pr: want create [-title T] [-body B] [-base BRANCH], or merge NUMBER|L-n")
	}
	resp, err := coveRequest(path, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || loc == "/login" {
		b, _ := io.ReadAll(resp.Body)
		if loc == "/login" {
			b = []byte("your GitCove token isn't valid")
		}
		return fmt.Errorf("GitCove said: %s", strings.TrimSpace(string(b)))
	}
	base, _ := gitOut("config", "--global", "--get", "cove.url")
	fmt.Printf("Done. It reaches GitHub after the outage: %s%s\n", base, loc)
	return nil
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
