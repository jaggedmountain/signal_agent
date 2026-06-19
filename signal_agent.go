// Signal Note-to-Self → Claude bridge.
//
// Runs as an unattended service. Drives a `signal-cli` JSON-RPC daemon, watches
// the linked account's Note-to-Self sync transcripts, and feeds each note to
// `claude -p` in a project-scoped cwd. The reply is posted back into the same
// Note-to-Self thread, so the phone becomes a remote for Claude on this host.
//
// Architecture in one diagram:
//
//	phone (Note to Self)
//	    │  Signal protocol
//	    ▼
//	signal-cli  ◀── jsonRpc stdio ──▶  Daemon (this program)
//	                                        │
//	                                        ├── receive loop:   notes → job channel
//	                                        ├── worker goroutine: claude -p in project cwd
//	                                        ├── stderr watchdog: reconnect-streak detection
//	                                        └── idle watchdog:   silent-WebSocket detection
//
// Note vocabulary (parsed in route() / parseCommand()):
//
//	@<project>           switch active project (sticky for subsequent notes)
//	@/   @default        snap active to SIGNAL_DEFAULT_PROJECT
//	@-                   swap active with previous
//	@?                   report current state (ack only)
//	@*                   list known projects (ack only)
//	@$$ / $$ / /$$       report Claude usage headroom (ack only)
//	/clear [prompt]      mint a fresh session in the active project
//	/!! prompt           run this single turn with --dangerously-skip-permissions
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
)

const workingEmoji = "⏳" // hourglass

// Watchdog: signal-cli's jsonRpc daemon sometimes ends up half-open after a
// reconnect — TCP says connected, no exception, but inbound sync messages stop
// arriving. Two failure modes, two signals:
//
// (a) "loud" half-open: a streak of "Connection closed unexpectedly" reconnect
//     warns with no intervening stdout traffic — signal-cli knows the
//     connection is bad and is retrying, but never recovers.
// (b) "silent" half-open: TCP says ESTAB, server has moved on, signal-cli sees
//     nothing — no warns at all, just dead air. Common when an upstream NAT or
//     firewall idle-times out the WebSocket without sending FIN/RST.
//
// Either way we exit nonzero so systemd's Restart=always kicks a fresh
// signal-cli process.
const (
	watchdogNeedle     = "Connection closed unexpectedly"
	watchdogThreshold  = 3                // consecutive warns (reset by any stdout traffic)
	watchdogMinElapsed = 60 * time.Second // avoids tripping on a fast transient burst
	idleCheckInterval  = 60 * time.Second
)

const (
	anthropicMessagesURL = "https://api.anthropic.com/v1/messages"
	oauthBeta            = "oauth-2025-04-20"
)

var (
	account              string
	signalCLI            string
	signalConfig         string
	signalAttachmentsDir string
	agentBin             string
	agentExtraArgs       []string
	agentTimeout         time.Duration
	projectsRoot         string
	claudeStore          string
	defaultProject       string
	startMs              int64
	credentialsPath      string
	usageModel           string
	usageTimeout         time.Duration
	idleReceiveThreshold time.Duration // 0 disables
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// --- config file ---
//
// On startup we load KEY=VALUE pairs from SIGNAL_AGENT_CONF (default
// $HOME/.config/signal_agent.env) into the process environment, so the binary
// is self-contained — no separate systemd EnvironmentFile required. Values
// already present in the environment win over the file, so ad-hoc overrides
// like `SIGNAL_ACCOUNT=+1... signal_agent` still work.

// Single source of truth for: the bootstrap template, the --env prompt order,
// and the regenerated file when --env saves. Description may be multi-line; it
// becomes both the on-screen help during --env and the `#`-comment block above
// each KEY in the file.
type configKey struct {
	name        string
	description string
	defaultFn   func() string
}

var configKeys = []configKey{
	{
		"SIGNAL_ACCOUNT",
		"REQUIRED: your own Signal number in E.164 format. Example: +15551234567",
		func() string { return "" },
	},
	{
		"SIGNAL_CLI",
		"signal-cli binary (default assumes ~/.local/bin is on PATH).",
		func() string { return "signal-cli" },
	},
	{
		"AGENT_BIN",
		"claude binary.",
		func() string { return "claude" },
	},
	{
		"SIGNAL_CONFIG",
		"signal-cli data dir. Pinned so the agent ignores the ambient XDG_DATA_HOME\n" +
			"(the VS Code snap, for example, redirects it and signal-cli then can't find\n" +
			"your linked account). This is where `signal-cli link` stored the account.",
		func() string { return filepath.Join(mustHome(), ".local/share/signal-cli") },
	},
	{
		"SIGNAL_PROJECTS_ROOT",
		"Root under which `@<project>` names resolve. e.g. `@shoebox` runs in\n" +
			"$SIGNAL_PROJECTS_ROOT/shoebox.",
		func() string { return filepath.Join(mustHome(), "projects") },
	},
	{
		"SIGNAL_DEFAULT_PROJECT",
		"Default project — used when no `@<project>` has switched us elsewhere, and\n" +
			"what `@/` / `@default` snap back to. Must be a folder name under\n" +
			"SIGNAL_PROJECTS_ROOT. Created automatically if missing.",
		func() string { return "signal_default" },
	},
	{
		"AGENT_TIMEOUT",
		"Seconds before a single agent run is killed.",
		func() string { return "1800" },
	},
	{
		"SIGNAL_IDLE_RECYCLE_SEC",
		"Recycle signal-cli if no inbound traffic for this many seconds. Catches the\n" +
			"silent half-open WebSocket case (TCP ESTAB but server stopped delivering).\n" +
			"Restart cost is ~3s. Set to 0 to disable.",
		func() string { return "7200" },
	},
	{
		"AGENT_EXTRA_ARGS",
		"Extra flags passed to `claude -p`. SECURITY: setting this lets the agent use\n" +
			"tools without interactive approval (it runs unattended, so it cannot prompt).\n" +
			"Only do this if you understand it can edit files and run commands on this host.\n" +
			"Examples:\n" +
			"  --dangerously-skip-permissions\n" +
			`  --allowedTools "Read" "Glob" "Grep"`,
		func() string { return "" },
	},
}

// configFlag is set by -c / --config in main. Resolved by configPath, which
// also checks SIGNAL_AGENT_CONF (env wins over the flag, flag wins over the
// default location).
var configFlag string

func configPath() string {
	if v := strings.TrimSpace(os.Getenv("SIGNAL_AGENT_CONF")); v != "" {
		return expandHome(v)
	}
	if v := strings.TrimSpace(configFlag); v != "" {
		return expandHome(v)
	}
	return filepath.Join(mustHome(), ".config", "signal_agent.env")
}

func defaultConfigValues() map[string]string {
	out := make(map[string]string, len(configKeys))
	for _, k := range configKeys {
		out[k.name] = k.defaultFn()
	}
	return out
}

// readConfigValues parses KEY=VALUE pairs from path. Comments and blank lines
// are skipped; a single matching pair of outer quotes is stripped, mirroring
// systemd's EnvironmentFile parser. Unknown keys are returned too — we don't
// silently drop anything the user typed.
func readConfigValues(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			logf("[config] %s:%d: skipping malformed line", path, lineno)
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[0] == val[len(val)-1] {
			val = val[1 : len(val)-1]
		}
		if key != "" {
			out[key] = val
		}
	}
	return out, sc.Err()
}

const configFilePreamble = `# signal_agent configuration
# Loaded at startup by signal_agent. Override the path with SIGNAL_AGENT_CONF.
# Re-run with --env to update this file interactively.
#
# Format: KEY=VALUE per line. Lines starting with # are ignored. Values from
# the surrounding environment take precedence over values set here.

`

// writeConfigFile renders values to path using configKeys for the layout and
// comment blocks. Always writes 0600 — the file holds your phone number.
func writeConfigFile(path string, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var sb strings.Builder
	sb.WriteString(configFilePreamble)
	for _, k := range configKeys {
		for _, line := range strings.Split(k.description, "\n") {
			sb.WriteString("# ")
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		sb.WriteString(k.name)
		sb.WriteByte('=')
		sb.WriteString(values[k.name])
		sb.WriteString("\n\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// loadConfig reads the conf file into the process environment. If the file is
// missing it writes a template and returns created=true so main can exit with
// a setup-instructions message.
func loadConfig() (path string, created bool, err error) {
	path = configPath()
	values, err := readConfigValues(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return path, false, err
		}
		if err := writeConfigFile(path, defaultConfigValues()); err != nil {
			return path, false, fmt.Errorf("write config template: %w", err)
		}
		return path, true, nil
	}
	for k, v := range values {
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		os.Setenv(k, v)
	}
	return path, false, nil
}

// envInteractive prompts for each configKey using the current file value (or
// the built-in default) as the prefill, then saves the result. Existing
// environment vars don't influence the prompts — this is about the file.
func envInteractive() error {
	path := configPath()
	existing, err := readConfigValues(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if existing == nil {
		existing = map[string]string{}
	}

	fmt.Fprintf(os.Stderr, "Configuring %s\nPress Enter to keep the shown value, or type a new one.\n\n", path)

	reader := bufio.NewReader(os.Stdin)
	values := make(map[string]string, len(configKeys))
	for _, k := range configKeys {
		cur, present := existing[k.name]
		if !present {
			cur = k.defaultFn()
		}
		display := cur
		if display == "" {
			display = "(empty)"
		}
		fmt.Fprintf(os.Stderr, "%s\n", k.name)
		for _, line := range strings.Split(k.description, "\n") {
			fmt.Fprintf(os.Stderr, "  %s\n", line)
		}
		fmt.Fprintf(os.Stderr, "  [%s] » ", display)

		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			values[k.name] = cur
		} else {
			values[k.name] = line
		}
		fmt.Fprintln(os.Stderr)
		if err == io.EOF {
			// User hit Ctrl-D mid-prompt; fill remaining keys with their
			// current/default and save.
			break
		}
	}
	// Any keys we didn't reach (Ctrl-D early): fill from existing/default.
	for _, k := range configKeys {
		if _, set := values[k.name]; !set {
			cur, present := existing[k.name]
			if !present {
				cur = k.defaultFn()
			}
			values[k.name] = cur
		}
	}

	if err := writeConfigFile(path, values); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved %s\n", path)
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func mustHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

func expandHome(p string) string {
	if p == "~" {
		return mustHome()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(mustHome(), p[2:])
	}
	return p
}

func ifEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// shlexSplit is a minimal POSIX-ish word splitter for AGENT_EXTRA_ARGS. Handles
// single quotes, double quotes, and backslash escapes outside single quotes.
func shlexSplit(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inSingle, inDouble, escape, inWord := false, false, false, false
	for _, r := range s {
		if escape {
			cur.WriteRune(r)
			escape = false
			inWord = true
			continue
		}
		if r == '\\' && !inSingle {
			escape = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			inWord = true
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			inWord = true
			continue
		}
		if unicode.IsSpace(r) && !inSingle && !inDouble {
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
			continue
		}
		cur.WriteRune(r)
		inWord = true
	}
	if inSingle || inDouble {
		return nil, errors.New("unbalanced quote")
	}
	if escape {
		return nil, errors.New("dangling escape")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

func initEnv() {
	account = strings.TrimSpace(os.Getenv("SIGNAL_ACCOUNT"))
	signalCLI = envOr("SIGNAL_CLI", "signal-cli")
	signalConfig = strings.TrimSpace(envOr("SIGNAL_CONFIG", filepath.Join(mustHome(), ".local/share/signal-cli")))
	signalAttachmentsDir = filepath.Join(signalConfig, "attachments")
	agentBin = envOr("AGENT_BIN", "claude")
	if extra, err := shlexSplit(os.Getenv("AGENT_EXTRA_ARGS")); err != nil {
		logf("[init] AGENT_EXTRA_ARGS: %v", err)
	} else {
		agentExtraArgs = extra
	}
	// Generous default — async means a stuck job no longer blocks intake, so the
	// timeout is just a safety net against truly hung agent processes.
	agentTimeout = time.Duration(envInt("AGENT_TIMEOUT", 1800)) * time.Second

	pr := envOr("SIGNAL_PROJECTS_ROOT", filepath.Join(mustHome(), "projects"))
	if rp, err := filepath.EvalSymlinks(pr); err == nil {
		projectsRoot = rp
	} else if abs, err := filepath.Abs(pr); err == nil {
		projectsRoot = abs
	} else {
		projectsRoot = pr
	}
	claudeStore = filepath.Join(mustHome(), ".claude/projects")
	startMs = time.Now().UnixMilli()

	credentialsPath = expandHome(envOr("SIGNAL_CLAUDE_CREDENTIALS", "~/.claude/.credentials.json"))
	usageModel = envOr("SIGNAL_USAGE_MODEL", "claude-haiku-4-5-20251001")
	usageTimeout = time.Duration(envInt("SIGNAL_USAGE_TIMEOUT", 15)) * time.Second
	idleReceiveThreshold = time.Duration(envFloat("SIGNAL_IDLE_RECYCLE_SEC", 7200) * float64(time.Second))

	defaultProject = resolveDefaultProject()
}

// --- state files ---

func stateDir() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	return filepath.Join(mustHome(), ".local/state")
}

func activePath() string   { return filepath.Join(stateDir(), "signal_agent", "active-project") }
func previousPath() string { return filepath.Join(stateDir(), "signal_agent", "previous-project") }

func readState(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeState(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value+"\n"), 0o644)
}

func resolveDefaultProject() string {
	name := strings.TrimSpace(envOr("SIGNAL_DEFAULT_PROJECT", "signal_default"))
	path := filepath.Join(projectsRoot, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		// Best-effort: keep the daemon up even if PROJECTS_ROOT is unwritable.
		// projectCwd still returns this path as a last resort.
		logf("[init] could not create default project dir %q: %v", path, err)
	}
	return name
}

func getActive() string {
	if v := readState(activePath()); v != "" {
		return v
	}
	return defaultProject
}

func getPrevious() string { return readState(previousPath()) }

// getProjectList: comma-separated project names with the active one marked '*'
// and the default marked '(default)'. Returns "(none)" when PROJECTS_ROOT is empty.
func getProjectList() string {
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return "(none)"
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	active := getActive()
	out := make([]string, 0, len(names))
	for _, n := range names {
		label := n
		if n == active {
			label += "*"
		}
		if n == defaultProject {
			label += " (default)"
		}
		out = append(out, label)
	}
	return strings.Join(out, ", ")
}

// setActive switches active project, demoting old to previous. No-op if unchanged.
func setActive(name string) bool {
	current := getActive()
	if name == current {
		return false
	}
	if err := writeState(previousPath(), current); err != nil {
		logf("[state] write previous failed: %v", err)
	}
	if err := writeState(activePath(), name); err != nil {
		logf("[state] write active failed: %v", err)
	}
	return true
}

// --- claude subprocess ---

func runClaude(ctx context.Context, prompt, sessionID, cwd, mode string, dangerous bool) (stdout, stderr string, exitCode int, err error) {
	args := []string{"-p"}
	var label string
	switch mode {
	case "continue":
		args = append(args, "--continue")
		label = "continue latest"
	case "resume":
		args = append(args, "--resume", sessionID)
		label = "resume " + sessionID
	default: // "create"
		args = append(args, "--session-id", sessionID)
		label = "create " + sessionID
	}
	if dangerous {
		args = append(args, "--dangerously-skip-permissions")
		label += " (skip-permissions)"
	}
	args = append(args, agentExtraArgs...)
	logf("[claude] %s in %s, %d chars", label, cwd, len(prompt))

	cmd := exec.CommandContext(ctx, agentBin, args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	runErr := cmd.Run()
	stdout = so.String()
	stderr = se.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	return stdout, stderr, -1, runErr
}

func askClaude(prompt, sessionID, cwd string, forceCreate, dangerous bool) string {
	// forceCreate=true → `/clear` or first turn in a fresh project: start with
	//                    `--session-id <new-uuid>`.
	// otherwise        → `--continue` in cwd (route guarantees history exists,
	//                    since first-turn-in-fresh-project sets forceCreate).
	// dangerous=true   → `!!` directive: append --dangerously-skip-permissions.
	mode := "continue"
	if forceCreate {
		mode = "create"
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
	defer cancel()
	stdout, stderr, exitCode, err := runClaude(ctx, prompt, sessionID, cwd, mode, dangerous)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("(claude timed out after %ds)", int(agentTimeout.Seconds()))
	}
	if err != nil {
		// Spawn-time failure: missing binary, missing cwd, etc. Include both so
		// the user can tell which path was the culprit.
		return fmt.Sprintf("(spawn failed: %v; binary=%q cwd=%q)", err, agentBin, cwd)
	}
	out := strings.TrimSpace(stdout)
	if exitCode != 0 {
		errMsg := strings.TrimSpace(stderr)
		if len(errMsg) > 500 {
			errMsg = errMsg[:500]
		}
		if out != "" {
			return out
		}
		return fmt.Sprintf("(claude exited %d: %s)", exitCode, errMsg)
	}
	if out == "" {
		return "(claude returned no output)"
	}
	return out
}

// encodeCwdForClaudeStore maps a filesystem path to Claude Code's transcript
// folder name. Empirically Claude Code replaces '/' and '_' with '-' in the
// project folder under ~/.claude/projects/. Other characters (incl. case,
// hyphens) appear to pass through. Only used as a "does any session exist
// here?" probe — if this encoding ever drifts, hasClaudeHistory returns false
// and the route silently falls back to the dedicated session.
func encodeCwdForClaudeStore(cwd string) string {
	s := strings.TrimLeft(cwd, "/")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return "-" + s
}

func hasClaudeHistory(cwd string) bool {
	dir := filepath.Join(claudeStore, encodeCwdForClaudeStore(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// --- routing ---

var routeRE = regexp.MustCompile(`(?s)^\s*@(\S+)\s*(.*)`)

// resolveProjectDir maps a user-supplied @<name> token to an actual project
// directory. Prefers an exact-case match; falls back to a case-insensitive scan
// of PROJECTS_ROOT because phone keyboards routinely auto-capitalize the first
// word. Returns the resolved absolute path or "".
//
// Rejects names with path separators or a leading dot so "@.." and
// "@../etc/passwd" can't escape PROJECTS_ROOT.
func resolveProjectDir(name string) string {
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) || strings.HasPrefix(name, ".") {
		return ""
	}
	exact := filepath.Join(projectsRoot, name)
	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		if rp, err := filepath.EvalSymlinks(exact); err == nil {
			return rp
		}
		return exact
	}
	entries, err := os.ReadDir(projectsRoot)
	if err != nil {
		return ""
	}
	needle := strings.ToLower(name)
	var matches []string
	for _, e := range entries {
		if e.IsDir() && strings.ToLower(e.Name()) == needle {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) == 0 {
		return ""
	}
	if len(matches) > 1 {
		logf("[route] @%s: ambiguous case-insensitive match %v; picking %q", name, matches, matches[0])
	}
	p := filepath.Join(projectsRoot, matches[0])
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	return p
}

// projectCwd resolves a project name to an absolute cwd. Falls back to the
// default project if `name` no longer exists (e.g., stale persisted state).
func projectCwd(name string) string {
	if c := resolveProjectDir(name); c != "" {
		return c
	}
	if name != defaultProject {
		logf("[route] project '%s' missing; falling back to default '%s'", name, defaultProject)
		if c := resolveProjectDir(defaultProject); c != "" {
			return c
		}
	}
	// Last resort: the default project's path, built directly so we always
	// return a real path even if it doesn't exist yet. Keeps the daemon up.
	return filepath.Join(projectsRoot, defaultProject)
}

type routeKind int

const (
	routeNormal routeKind = iota
	routeReport                // @? — status only
	routeNoPrevious            // @- but nothing to swap to
	routeList                  // @*
	routeUsage                 // @$$ / $$ — usage headroom
)

type routeResult struct {
	kind       routeKind
	cwd        string
	body       string
	switchedTo string // "" = no project switch
}

// route resolves a note against the active-project state.
func route(text string) routeResult {
	current := getActive()
	m := routeRE.FindStringSubmatch(text)
	if m == nil {
		return routeResult{kind: routeNormal, cwd: projectCwd(current), body: text}
	}
	token, body := m[1], m[2]

	switch token {
	case "?":
		return routeResult{kind: routeReport}
	case "*":
		return routeResult{kind: routeList}
	case "$$":
		return routeResult{kind: routeUsage}
	}

	var target string
	switch {
	case token == "/" || strings.EqualFold(token, "default"):
		target = defaultProject
	case token == "-":
		prev := getPrevious()
		if prev == "" {
			return routeResult{kind: routeNoPrevious}
		}
		target = prev
	default:
		candidate := resolveProjectDir(token)
		if candidate == "" {
			logf("[route] @%s: not a project under %s; staying on '%s'", token, projectsRoot, current)
			return routeResult{kind: routeNormal, cwd: projectCwd(current), body: text}
		}
		target = filepath.Base(candidate)
	}

	if setActive(target) {
		logf("[switch] '%s' → '%s'", current, target)
	} else {
		logf("[route] @%s: already on '%s'", token, target)
	}
	return routeResult{kind: routeNormal, cwd: projectCwd(target), body: body, switchedTo: target}
}

// handleClear mints a fresh UUID for a `/clear` directive. The session lives in
// the caller's cwd, which is the currently-active project; future notes in that
// scope continue it via `--continue`.
func handleClear() string {
	sid := newUUID()
	logf("[clear] new session %s", sid)
	return sid
}

var cmdRE = regexp.MustCompile(`(?s)^\s*/(\w+)\b\s*(.*)`)

// Symbolic directives that aren't /<word>: `!!` runs the turn with permissions
// skipped, `$$` reports usage headroom (ack-only).
var sigils = []string{"!!", "$$"}

// parseCommand pulls a leading directive off text. Recognizes the `!!` / `$$`
// sigils (with or without a leading `/`, since phone muscle memory types `/$$`)
// and /<word> commands. Returns (cmd, remainder) or ("", text); cmd is the
// sigil itself or the lowercased word.
func parseCommand(text string) (cmd, rest string) {
	stripped := strings.TrimLeftFunc(text, unicode.IsSpace)
	sig := stripped
	if strings.HasPrefix(sig, "/") {
		sig = sig[1:]
	}
	for _, s := range sigils {
		if strings.HasPrefix(sig, s) {
			return s, strings.TrimLeftFunc(sig[len(s):], unicode.IsSpace)
		}
	}
	m := cmdRE.FindStringSubmatch(text)
	if m == nil {
		return "", text
	}
	return strings.ToLower(m[1]), m[2]
}

// --- envelope parsing ---

type attachment struct {
	ID          string `json:"id"`
	ContentType string `json:"contentType,omitempty"`
}

type sentMessage struct {
	DestinationNumber string       `json:"destinationNumber,omitempty"`
	Destination       string       `json:"destination,omitempty"`
	GroupInfo         any          `json:"groupInfo,omitempty"`
	Message           string       `json:"message,omitempty"`
	Timestamp         int64        `json:"timestamp,omitempty"`
	Attachments       []attachment `json:"attachments,omitempty"`
}

type syncMessage struct {
	SentMessage *sentMessage `json:"sentMessage,omitempty"`
}

type envelope struct {
	Timestamp   int64        `json:"timestamp,omitempty"`
	SyncMessage *syncMessage `json:"syncMessage,omitempty"`
}

type note struct {
	text        string
	attachments []attachment
	ts          int64
}

func extractNoteToSelf(env *envelope) (note, bool) {
	if env.SyncMessage == nil || env.SyncMessage.SentMessage == nil {
		return note{}, false
	}
	sm := env.SyncMessage.SentMessage
	dest := sm.DestinationNumber
	if dest == "" {
		dest = sm.Destination
	}
	if dest != account {
		return note{}, false
	}
	if sm.GroupInfo != nil {
		return note{}, false
	}
	text := strings.TrimSpace(sm.Message)
	if text == "" && len(sm.Attachments) == 0 {
		return note{}, false
	}
	ts := sm.Timestamp
	if ts == 0 {
		ts = env.Timestamp
	}
	return note{text: text, attachments: sm.Attachments, ts: ts}, true
}

func resolveAttachments(atts []attachment) []string {
	var paths []string
	for _, a := range atts {
		if a.ID == "" {
			logf("[attach] missing source for (empty id)")
			continue
		}
		src := filepath.Join(signalAttachmentsDir, a.ID)
		if _, err := os.Stat(src); err != nil {
			logf("[attach] missing source for %s", a.ID)
			continue
		}
		paths = append(paths, src)
		ct := a.ContentType
		if ct == "" {
			ct = "?"
		}
		logf("[attach] %s %s", ct, src)
	}
	return paths
}

func buildPrompt(text string, files []string) string {
	if len(files) == 0 {
		return text
	}
	var sb strings.Builder
	if text != "" {
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}
	sb.WriteString("Attached file(s):\n")
	for i, p := range files {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("- ")
		sb.WriteString(p)
	}
	return sb.String()
}

// --- usage report ($$ data source) ---
//
// The claude CLI exposes no usage subcommand, and the panel behind `/usage` is
// "nonessential traffic" (disabled in some setups) that only fires
// interactively. The same numbers ride on the `anthropic-ratelimit-unified-*`
// response headers of any /v1/messages call, so we make a tiny throwaway call
// with the subscription OAuth token and read the 5h + weekly utilization. Cost
// is negligible (Haiku, max_tokens=1); on a throttled (429) response no tokens
// are spent and the headers are still present. Best-effort: any failure
// degrades to a placeholder rather than raising.

func oauthToken() (string, error) {
	b, err := os.ReadFile(credentialsPath)
	if err != nil {
		return "", err
	}
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	if c.ClaudeAiOauth.AccessToken == "" {
		return "", errors.New("no accessToken in credentials")
	}
	return c.ClaudeAiOauth.AccessToken, nil
}

func fmtReset(epoch int64, withDay bool) string {
	t := time.Unix(epoch, 0).Local()
	hm := t.Format("3:04PM")
	// "4:30PM" -> "4:30pm"
	if len(hm) >= 2 {
		hm = hm[:len(hm)-2] + strings.ToLower(hm[len(hm)-2:])
	}
	if withDay {
		return t.Format("Mon ") + hm
	}
	return hm
}

func formatUsage(h http.Header) string {
	window := func(label, key string, withDay bool) string {
		util := h.Get("anthropic-ratelimit-unified-" + key + "-utilization")
		if util == "" {
			return ""
		}
		f, err := strconv.ParseFloat(util, 64)
		if err != nil {
			return ""
		}
		pct := fmt.Sprintf("%.0f%%", f*100)
		resetStr := h.Get("anthropic-ratelimit-unified-" + key + "-reset")
		if resetStr == "" {
			return fmt.Sprintf("%s: %s", label, pct)
		}
		resetEpoch, err := strconv.ParseInt(resetStr, 10, 64)
		if err != nil {
			return fmt.Sprintf("%s: %s", label, pct)
		}
		return fmt.Sprintf("%s: %s (resets %s)", label, pct, fmtReset(resetEpoch, withDay))
	}
	var parts []string
	if p := window("5h", "5h", false); p != "" {
		parts = append(parts, p)
	}
	if p := window("week", "7d", true); p != "" {
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "(usage unavailable: no rate-limit headers)"
	}
	status := h.Get("anthropic-ratelimit-unified-status")
	if status != "" && status != "allowed" {
		parts = append(parts, "status: "+status)
	}
	return strings.Join(parts, " · ")
}

// usageReport: `/usage`-style headroom string for the `$$` directive: percent
// used of the 5-hour and weekly rate-limit windows, with reset times.
// Best-effort.
func usageReport() string {
	token, err := oauthToken()
	if err != nil {
		return fmt.Sprintf("(usage unavailable: no OAuth token: %v)", err)
	}
	body, _ := json.Marshal(map[string]any{
		"model":      usageModel,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "."}},
	})
	req, err := http.NewRequest(http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("(usage unavailable: %v)", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: usageTimeout}
	resp, err := client.Do(req)
	if err != nil {
		msg := err.Error()
		if len(msg) > 120 {
			msg = msg[:120]
		}
		return fmt.Sprintf("(usage unavailable: %s)", msg)
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused. Body is irrelevant; only headers matter.
	io.Copy(io.Discard, resp.Body)
	return formatUsage(resp.Header)
}

// --- UUID v4 ---

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// --- daemon ---

type job struct {
	prompt      string
	targetTs    int64
	cwd         string
	sessionID   string
	forceCreate bool
	dangerous   bool
}

type daemon struct {
	proc   *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	reqID   atomic.Int64
	stdinMu sync.Mutex
	jobs    chan job

	streakMu        sync.Mutex
	warnStreak      int
	warnStreakStart time.Time

	lastStdoutNs atomic.Int64
}

func newDaemon() *daemon {
	d := &daemon{jobs: make(chan job, 256)}
	d.lastStdoutNs.Store(time.Now().UnixNano())
	return d
}

func (d *daemon) start() error {
	args := []string{}
	if signalConfig != "" {
		args = append(args, "--config", signalConfig)
	}
	args = append(args, "-a", account, "jsonRpc")
	logf("[signal] starting: %s %s", signalCLI, strings.Join(args, " "))
	cmd := exec.Command(signalCLI, args...)
	sin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	sout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	serr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	d.proc = cmd
	d.stdin = sin
	d.stdout = sout
	d.stderr = serr
	return nil
}

type rpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      string `json:"id"`
}

func (d *daemon) nextID() string {
	return strconv.FormatInt(d.reqID.Add(1), 10)
}

func (d *daemon) rpc(method string, params any) error {
	req := rpcRequest{Jsonrpc: "2.0", Method: method, Params: params, ID: d.nextID()}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	d.stdinMu.Lock()
	defer d.stdinMu.Unlock()
	_, err = d.stdin.Write(line)
	return err
}

func (d *daemon) sendMessage(text string) error {
	return d.rpc("send", map[string]any{
		"recipient": []string{account},
		"message":   text,
	})
}

func (d *daemon) sendReaction(targetTs int64, emoji string, remove bool) error {
	p := map[string]any{
		"recipient":       []string{account},
		"emoji":           emoji,
		"targetAuthor":    account,
		"targetTimestamp": targetTs,
	}
	if remove {
		p["remove"] = true
	}
	return d.rpc("sendReaction", p)
}

// stderrWatchdog forwards signal-cli stderr to our journal and watches for the
// half-open reconnect pattern. Exits the whole process when N reconnect warns
// accumulate with no intervening stdout traffic.
func (d *daemon) stderrWatchdog() {
	sc := bufio.NewScanner(d.stderr)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line != "" {
			logf("[signal-cli] %s", line)
		}
		if !strings.Contains(line, watchdogNeedle) {
			continue
		}
		d.streakMu.Lock()
		if d.warnStreak == 0 {
			d.warnStreakStart = time.Now()
		}
		d.warnStreak++
		n := d.warnStreak
		start := d.warnStreakStart
		d.streakMu.Unlock()

		elapsed := time.Since(start)
		if n >= watchdogThreshold && elapsed >= watchdogMinElapsed {
			logf("[watchdog] %d reconnect warns over %.0fs with no stdout traffic; exiting for systemd to restart",
				n, elapsed.Seconds())
			_ = d.proc.Process.Signal(syscall.SIGTERM)
			os.Exit(2)
		}
	}
}

// idleWatchdog catches the silent half-open case: WebSocket says ESTAB but
// signal-cli receives nothing. Wakes periodically and exits when stdout has
// been quiet longer than idleReceiveThreshold.
func (d *daemon) idleWatchdog() {
	if idleReceiveThreshold <= 0 {
		return
	}
	for {
		time.Sleep(idleCheckInterval)
		last := time.Unix(0, d.lastStdoutNs.Load())
		idle := time.Since(last)
		if idle >= idleReceiveThreshold {
			logf("[watchdog] no stdout traffic for %.0fs (>%.0fs) — likely ghost socket; exiting for systemd to restart",
				idle.Seconds(), idleReceiveThreshold.Seconds())
			_ = d.proc.Process.Signal(syscall.SIGTERM)
			os.Exit(3)
		}
	}
}

func (d *daemon) workerLoop() {
	for j := range d.jobs {
		if err := d.sendReaction(j.targetTs, workingEmoji, false); err != nil {
			logf("[signal] reaction add failed: %v", err)
		}
		reply := askClaude(j.prompt, j.sessionID, j.cwd, j.forceCreate, j.dangerous)
		if err := d.sendReaction(j.targetTs, workingEmoji, true); err != nil {
			logf("[signal] reaction remove failed: %v", err)
		}
		if err := d.sendMessage(reply); err != nil {
			logf("[signal] pipe closed while sending; exiting worker: %v", err)
			return
		}
		logf("[sent] %d chars", len(reply))
	}
}

func (d *daemon) run() int {
	if err := d.start(); err != nil {
		logf("[signal] start failed: %v", err)
		return 1
	}

	go d.workerLoop()
	go d.stderrWatchdog()
	go d.idleWatchdog()

	sc := bufio.NewScanner(d.stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Any stdout traffic = signal-cli is actually delivering events,
		// so the warn streak resets and idle timer is bumped.
		d.streakMu.Lock()
		d.warnStreak = 0
		d.streakMu.Unlock()
		d.lastStdoutNs.Store(time.Now().UnixNano())

		var msg struct {
			Method string `json:"method"`
			Params struct {
				Envelope envelope `json:"envelope"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Method != "receive" {
			continue
		}
		n, ok := extractNoteToSelf(&msg.Params.Envelope)
		if !ok {
			continue
		}
		if n.ts != 0 && n.ts < startMs {
			logf("[skip] backlog message ts=%d < start=%d", n.ts, startMs)
			continue
		}
		depth := len(d.jobs) + 1
		runes := []rune(n.text)
		preview := n.text
		if len(runes) > 120 {
			preview = string(runes[:120])
		}
		logf("[note] %q (queued, depth=%d)", preview, depth)

		rr := route(n.text)

		switch rr.kind {
		case routeReport:
			d.sendMessage(fmt.Sprintf("(active: %s, previous: %s)", getActive(), ifEmpty(getPrevious(), "—")))
			continue
		case routeNoPrevious:
			d.sendMessage(fmt.Sprintf("(no previous project; active: %s)", getActive()))
			continue
		case routeList:
			d.sendMessage("Existing projects: " + getProjectList())
			continue
		case routeUsage:
			d.sendMessage(usageReport())
			continue
		}

		cwd := rr.cwd
		body := rr.body
		// Bare project switch with no body — just ack.
		if rr.switchedTo != "" && strings.TrimSpace(body) == "" {
			d.sendMessage(fmt.Sprintf("(active: %s)", rr.switchedTo))
			continue
		}

		cmd, afterCmd := parseCommand(body)
		var sessionID string
		forceCreate := false
		dangerous := false

		// `$$` — ack-only usage report; never calls claude.
		if cmd == "$$" {
			d.sendMessage(usageReport())
			continue
		}

		if cmd == "!!" {
			// Run this turn with permissions skipped. The remainder is the
			// prompt; nothing to run without one.
			if strings.TrimSpace(afterCmd) == "" {
				d.sendMessage("(nothing to run)")
				continue
			}
			dangerous = true
			body = afterCmd
		}

		if cmd == "clear" {
			sessionID = handleClear()
			forceCreate = true
			// Without a follow-up message there's nothing for claude to
			// respond to; a minimal stand-in still gets the session file
			// written so the next note in this scope continues it.
			body = strings.TrimSpace(afterCmd)
			if body == "" {
				body = "(new session — wait for my next note)"
			}
		} else if !hasClaudeHistory(cwd) {
			// First turn in a project that doesn't have a Claude session yet.
			// --continue would fail; bootstrap with a fresh UUID.
			sessionID = newUUID()
			forceCreate = true
			logf("[route] no history in %s; bootstrapping %s", cwd, sessionID)
		}

		files := resolveAttachments(n.attachments)
		d.jobs <- job{
			prompt:      buildPrompt(body, files),
			targetTs:    n.ts,
			cwd:         cwd,
			sessionID:   sessionID,
			forceCreate: forceCreate,
			dangerous:   dangerous,
		}
	}
	if err := sc.Err(); err != nil {
		logf("[signal] stdout scan error: %v", err)
	}

	err := d.proc.Wait()
	rc := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			rc = exitErr.ExitCode()
		} else {
			rc = 1
		}
	}
	if rc == 0 {
		rc = 1
	}
	logf("[signal] daemon exited rc=%d", rc)
	return rc
}

func main() {
	envMode := flag.Bool("env", false, "Interactively edit the config file and exit.")
	flag.StringVar(&configFlag, "c", "", "Config file path. Overridden by $SIGNAL_AGENT_CONF; defaults to ~/.config/signal_agent.env.")
	flag.StringVar(&configFlag, "config", "", "Alias for -c.")
	flag.Parse()

	if *envMode {
		if err := envInteractive(); err != nil {
			logf("ERROR: %v", err)
			os.Exit(2)
		}
		os.Exit(0)
	}

	confPath, created, err := loadConfig()
	if err != nil {
		logf("ERROR: loading %s: %v", confPath, err)
		os.Exit(2)
	}
	if created {
		logf("[init] created config template at %s", confPath)
		logf("[init] edit it (or re-run with --env) to set SIGNAL_ACCOUNT, then re-run.")
		os.Exit(0)
	}

	initEnv()
	if account == "" {
		logf("ERROR: SIGNAL_ACCOUNT is required (your own +number). Set it in %s.", confPath)
		os.Exit(2)
	}
	idleStr := "off"
	if idleReceiveThreshold > 0 {
		idleStr = fmt.Sprintf("%.0fs", idleReceiveThreshold.Seconds())
	}
	logf("[init] conf=%s account=%s config=%s projects=%s default=%s active=%s previous=%s timeout=%ds reaction=%s watchdog=%dwarns/%.0fs idle-recycle=%s",
		confPath, account, ifEmpty(signalConfig, "(default)"), projectsRoot, defaultProject,
		getActive(), ifEmpty(getPrevious(), "—"),
		int(agentTimeout.Seconds()), workingEmoji,
		watchdogThreshold, watchdogMinElapsed.Seconds(), idleStr)

	os.Exit(newDaemon().run())
}
