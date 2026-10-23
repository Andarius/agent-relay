package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

const (
	tailnetStartURL = "https://login.tailscale.com/start"
	apiKeysURL      = "https://login.tailscale.com/admin/settings/keys"
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle   = lipgloss.NewStyle().Faint(true)
	warnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	boxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

// initAnswers is what the wizard collects; flags fill the same fields for --yes runs.
type initAnswers struct {
	Mode      string // join | host
	Invite    string
	Token     string
	SaveToken bool
	Handle    string
	Name      string
	Workspace string
	Targets   []string // sbx, claude, codex
	CloseOpen bool
	Bypass    bool
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	yes := fs.Bool("yes", false, "no prompts: use flags, environment and defaults")
	host := fs.Bool("host", false, "start or extend your own agent network (API token from $TS_API_KEY)")
	join := fs.Bool("join", false, "join with an invite read from $AGENT_RELAY_INVITE or stdin")
	name := fs.String("name", "", "this machine's agent name")
	handle := fs.String("handle", "", "your handle; your machines get tag:agent-<handle>")
	workspace := fs.String("workspace", "", "folder your agent works in")
	var targets listFlag
	fs.Var(&targets, "agent", "where to attach the relay: sbx, or claude/codex with --bypass; repeatable (default: detected)")
	bypass := fs.Bool("bypass", false, "allow an agent outside an sbx sandbox (Claude Code or Codex on this machine)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "agent-relay init: guided setup. With --yes, pass --host or --join and the flags below.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	prev, _ := LoadConfig()
	a := initAnswers{Name: *name, Handle: *handle, Workspace: *workspace, Targets: targets, Bypass: *bypass}
	switch {
	case *host:
		a.Mode = "host"
	case *join:
		a.Mode = "join"
	}
	fillDefaults(&a, prev)
	// Fail before any invite, key or policy change is spent.
	base := requestBase(&a)
	if err := requireSandbox(base.Sandbox, base.Clients, a.Bypass); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if *yes {
		return initNonInteractive(ctx, &a, prev)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("init needs a terminal; use --yes with flags for scripted setups")
	}
	fmt.Println(titleStyle.Render("agent-relay") + dimStyle.Render("  let your agents message other agents over Tailscale"))
	return runWizard(ctx, &a, prev)
}

var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 41 {
		s = strings.TrimRight(s[:41], "-")
	}
	return s
}

func fillDefaults(a *initAnswers, prev *Config) {
	home, _ := os.UserHomeDir()
	if prev != nil {
		if a.Name == "" {
			a.Name = prev.Name
		}
		if a.Workspace == "" {
			a.Workspace = prev.Workspace
		}
		for _, t := range prev.Tags {
			if h, ok := strings.CutPrefix(t, agentTagPrefix); ok && a.Handle == "" {
				a.Handle = h
			}
		}
	}
	if a.Name == "" {
		h, _ := os.Hostname()
		a.Name = slug(strings.Split(h, ".")[0])
	}
	if a.Handle == "" {
		a.Handle = slug(os.Getenv("USER"))
	}
	if a.Workspace == "" {
		a.Workspace = filepath.Join(home, "agent-relay", "ws")
		if h := os.Getenv("AGENT_RELAY_HOME"); h != "" {
			a.Workspace = filepath.Join(h, "ws")
		}
	}
	if len(a.Targets) == 0 {
		a.Targets = detectedTargets(true)
	}
	a.Token = loadAPIToken()
}

// detectedTargets lists installed agent runners; preferred keeps only the sandbox when sbx exists.
func detectedTargets(preferred bool) []string {
	var out []string
	for _, t := range []string{"sbx", "claude", "codex"} {
		if _, err := exec.LookPath(t); err == nil {
			out = append(out, t)
		}
	}
	if preferred && slices.Contains(out, "sbx") {
		return []string{"sbx"}
	}
	if preferred && len(out) > 1 {
		return out[:1]
	}
	return out
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

func validName(s string) error {
	if !nameRe.MatchString(s) {
		return errors.New("lowercase letters, digits and dashes")
	}
	return nil
}

// Wizard

func runWizard(ctx context.Context, a *initAnswers, prev *Config) error {
	if a.Mode == "" {
		if err := huh.NewSelect[string]().Title("How do you want to start?").Options(
			huh.NewOption("I have an invite", "join"),
			huh.NewOption("Start my own agent network (or add this machine to it)", "host"),
		).Height(4).Value(&a.Mode).Run(); err != nil {
			return err
		}
	}
	if a.Mode == "join" {
		return wizardJoin(ctx, a, prev)
	}
	return wizardHost(ctx, a, prev)
}

func targetField(a *initAnswers) huh.Field {
	found := detectedTargets(false)
	if !a.Bypass {
		found = slices.DeleteFunc(found, func(t string) bool { return t != "sbx" })
	}
	if len(found) == 0 {
		return huh.NewNote().Title("No agent found").
			Description("Install Claude Code, Codex or Docker Sandboxes (sbx), then run init again. The relay works without them.")
	}
	labels := map[string]string{
		"sbx":    "Docker sandbox (sbx): Claude runs isolated, recommended",
		"claude": "Claude Code on this machine (workspace folder only)",
		"codex":  "Codex on this machine (all Codex sessions)",
	}
	var opts []huh.Option[string]
	for _, t := range found {
		opts = append(opts, huh.NewOption(labels[t], t).Selected(slices.Contains(a.Targets, t)))
	}
	return huh.NewMultiSelect[string]().Title("Attach the relay to").Options(opts...).Value(&a.Targets)
}

func machineFields(a *initAnswers, askName bool) []huh.Field {
	var fields []huh.Field
	if askName {
		fields = append(fields, huh.NewInput().Title("This machine's agent name").
			Description("Friends see it in list_peers.").Value(&a.Name).Validate(validName))
	}
	return append(fields,
		huh.NewInput().Title("Workspace folder").Description("Where the agent works; it gets AGENTS.md and the inbox.").
			Value(&a.Workspace).Validate(func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("required")
			}
			return nil
		}),
		targetField(a),
	)
}

func wizardJoin(ctx context.Context, a *initAnswers, prev *Config) error {
	var inv Invite
	if err := huh.NewInput().Title("Paste the invite").Description("It starts with " + invitePrefix).
		EchoMode(huh.EchoModePassword).Value(&a.Invite).Validate(func(s string) error {
		var err error
		inv, err = DecodeInvite(s)
		return err
	}).Run(); err != nil {
		return err
	}
	a.Name = inv.Name
	summary := fmt.Sprintf("You join as %s and can message %s.", inv.Name, inv.From)
	if inv.AuthKey == "" {
		summary += "\nThe invite has no key: this machine must already be on your friend's tailnet."
	}
	fields := append([]huh.Field{huh.NewNote().Title("Invite from " + inv.From).Description(summary)}, machineFields(a, false)...)
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return err
	}
	return applyJoin(ctx, a, inv, prev)
}

func wizardHost(ctx context.Context, a *initAnswers, prev *Config) error {
	if a.Token == "" {
		var open bool
		if err := huh.NewForm(huh.NewGroup(
			huh.NewNote().Title("Your agent network").Description(
				"Agents meet on a tailnet of their own, so friends never see your devices.\n\n"+
					"1. Create it: sign in at "+tailnetStartURL+" with an account you don't use for your devices.\n"+
					"2. Open "+apiKeysURL+" in that tailnet.\n"+
					"   Under \"API access tokens\" (not \"Auth keys\"), click \"Generate access token…\".\n"+
					"   Description: agent-relay. Expiry: 90 days (the maximum); after that, init asks for a new one.\n"+
					"   Click Generate and copy the token right away: Tailscale shows it only once.\n\n"+
					"Already have one? Just paste its token next."),
			huh.NewConfirm().Title("Open these pages in your browser?").Value(&open),
		)).Run(); err != nil {
			return err
		}
		if open {
			for _, u := range []string{tailnetStartURL, apiKeysURL} {
				if !openBrowser(u) {
					fmt.Println(warnStyle.Render("Couldn't open a browser; visit " + u))
				}
			}
		}
		a.SaveToken = true
	}
	api := NewTSAPI(a.Token)
	var devices []Device
	for {
		if a.Token == "" {
			if err := huh.NewForm(huh.NewGroup(
				huh.NewInput().Title("API access token").Description("Starts with tskey-api-. Lost it? Generate a new one, the old one keeps working until it expires.").EchoMode(huh.EchoModePassword).
					Value(&a.Token).Validate(func(s string) error {
					if !strings.HasPrefix(strings.TrimSpace(s), "tskey-api-") {
						return errors.New("an API access token starts with tskey-api-")
					}
					return nil
				}),
				huh.NewConfirm().Title("Remember it on this machine?").
					Description("Lets `agent-relay invite` mint keys later. Stored 0600 in your config folder.").Value(&a.SaveToken),
			)).Run(); err != nil {
				return err
			}
		}
		api.Token = strings.TrimSpace(a.Token)
		var err error
		if devices, err = api.Devices(ctx); err == nil {
			break
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			return err
		}
		fmt.Println(warnStyle.Render("✗ " + err.Error()))
		a.Token, a.SaveToken = "", true
	}
	a.Token = api.Token
	fields := append([]huh.Field{huh.NewInput().Title("Your handle").
		Description("Your machines get the tag tag:agent-<handle>.").Value(&a.Handle).Validate(validName)},
		machineFields(a, true)...)
	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return err
	}

	tag := agentTagPrefix + a.Handle
	cur, _, err := api.Policy(ctx)
	if err != nil {
		return err
	}
	dedicated := isDedicated(devices)
	a.CloseOpen = dedicated
	preview, err := planPolicy(cur, []string{tag}, a.CloseOpen)
	if err != nil {
		return err
	}
	if preview.OpenRules > 0 {
		desc := "The policy lets every device reach every other. Agents only need their relay port, so closing it is safer."
		if !dedicated {
			desc = warnStyle.Render("This tailnet has devices that are not agent relays.") +
				"\nIf you keep allow-all, friends' relays can reach them. Use a dedicated tailnet instead, or close it."
		}
		if err := huh.NewConfirm().Title("Replace the allow-all rule with relay-only access?").
			Description(desc).Value(&a.CloseOpen).Run(); err != nil {
			return err
		}
		if !a.CloseOpen && !dedicated {
			return errors.New("stopped: use a tailnet dedicated to agents, or allow init to close the allow-all rule")
		}
	}
	return applyHost(ctx, a, api, devices, prev, true)
}

// isDedicated reports whether every device on the tailnet is an agent relay.
func isDedicated(devices []Device) bool {
	for _, d := range devices {
		if !slices.ContainsFunc(d.Tags, func(t string) bool { return strings.HasPrefix(t, agentTagPrefix) }) {
			return false
		}
	}
	return true
}

// Apply

func requestBase(a *initAnswers) SetupRequest {
	req := SetupRequest{Name: a.Name, Workspace: expandHome(strings.TrimSpace(a.Workspace)), Peers: map[string]Peer{}, Bypass: a.Bypass}
	for _, t := range a.Targets {
		if t == "sbx" {
			req.Sandbox = "relay-" + a.Name
		} else {
			req.Clients = append(req.Clients, t)
		}
	}
	return req
}

func keepPeers(req *SetupRequest, prev *Config) {
	if prev == nil {
		return
	}
	for n, p := range prev.Peers {
		if _, ok := req.Peers[n]; !ok && n != req.Name {
			req.Peers[n] = p
		}
	}
}

func applyJoin(ctx context.Context, a *initAnswers, inv Invite, prev *Config) error {
	base := requestBase(a)
	req := RequestFromInvite(inv, base.Workspace, base.Sandbox)
	req.Clients, req.Bypass = base.Clients, base.Bypass
	keepPeers(&req, prev)
	if err := runSetup(ctx, req); err != nil {
		return err
	}
	printNext(req)
	return nil
}

func applyHost(ctx context.Context, a *initAnswers, api *TSAPI, devices []Device, prev *Config, interactive bool) error {
	tag := agentTagPrefix + a.Handle
	req := requestBase(a)
	if err := requireSandbox(req.Sandbox, req.Clients, req.Bypass); err != nil {
		return err
	}
	prog := &Progress{echo: true}
	plan, err := api.EnsureTags(ctx, []string{tag}, a.CloseOpen)
	if err != nil {
		return err
	}
	if plan.Changed() {
		prog.Add("tailnet policy updated: %s can reach each other's relay port", strings.Join(plan.GrantTags, ", "))
	}
	if a.SaveToken {
		if err := saveAPIToken(a.Token); err != nil {
			return err
		}
		prog.Add("API token saved for later invites")
	}

	req.Tags = []string{tag}
	keepPeers(&req, prev)
	hostname := "relay-" + a.Name
	_, statErr := os.Stat(filepath.Join(stateDirOf(prev), "tsnet"))
	reuse := prev != nil && prev.Name == a.Name && slices.Contains(prev.Tags, tag) && statErr == nil
	if !reuse {
		for _, d := range devices {
			if d.ShortName() == hostname && slices.Contains(d.Tags, tag) {
				if err := api.DeleteDevice(ctx, d.ID); err != nil {
					return err
				}
				prog.Add("removed the stale %s node from the tailnet", hostname)
			}
		}
		key, err := api.CreateKey(ctx, []string{tag}, "agent-relay "+a.Name, time.Hour)
		if err != nil {
			return err
		}
		req.AuthKey, req.NewNode = key, true
	}
	if err := runSetup(ctx, req); err != nil {
		return err
	}
	if interactive {
		if err := inviteLoop(ctx, api); err != nil {
			return err
		}
	}
	printNext(req)
	return nil
}

func stateDirOf(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.StateDir
}

func runSetup(ctx context.Context, req SetupRequest) error {
	prog := &Progress{echo: true}
	cfg, run, err := ApplySetup(ctx, req, prog)
	if err != nil {
		return err
	}
	defer run.Close()
	return FinishSetup(ctx, cfg, run, prog)
}

func printNext(req SetupRequest) {
	fmt.Println()
	fmt.Println(titleStyle.Render("Ready.") + " Start an agent session with the relay up:")
	fmt.Println("  agent-relay run")
	if req.Sandbox != "" {
		fmt.Println(dimStyle.Render("  First time: log Claude in inside the sandbox when it asks."))
	}
}

// Invites

// inviteWithKey adds name as a peer and mints a single-use key for tag, adding the tag to the policy if new.
func inviteWithKey(ctx context.Context, api *TSAPI, cfg *Config, name, tag string) (Invite, error) {
	if !slices.Contains(cfg.Tags, tag) {
		if _, err := api.EnsureTags(ctx, []string{tag}, false); err != nil {
			return Invite{}, err
		}
	}
	// A node already holding relay-<name> makes Tailscale suffix the joiner's name, which peers then reject.
	host := "relay-" + name
	devices, err := api.Devices(ctx)
	if err != nil {
		return Invite{}, err
	}
	for _, d := range devices {
		if d.ShortName() != host {
			continue
		}
		if !slices.Contains(d.Tags, tag) {
			return Invite{}, fmt.Errorf("the tailnet already has a %s node not tagged %s: remove or rename it in the admin console first", host, tag)
		}
		if err := api.DeleteDevice(ctx, d.ID); err != nil {
			return Invite{}, err
		}
		fmt.Fprintf(os.Stderr, "removed the previous %s node from the tailnet\n", host)
	}
	key, err := api.CreateKey(ctx, []string{tag}, "agent-relay "+name, 7*24*time.Hour)
	if err != nil {
		return Invite{}, err
	}
	return MakeInvite(cfg, Peer{Tag: tag}, name, key)
}

func inviteLoop(ctx context.Context, api *TSAPI) error {
	for {
		who, name := "mine", ""
		// Explicit heights: a standalone huh select otherwise draws only the selected row.
		if err := huh.NewSelect[string]().Title("Invite someone now?").Options(
			huh.NewOption("Another machine of mine", "mine"),
			huh.NewOption("A friend", "friend"),
			huh.NewOption("Done", ""),
		).Height(5).Value(&who).Run(); err != nil || who == "" {
			return err
		}
		title := "Friend's name"
		if who == "mine" {
			title = "That machine's agent name"
		}
		if err := huh.NewInput().Title(title).Value(&name).Validate(validName).Run(); err != nil {
			return err
		}
		cfg, err := LoadConfig()
		if err != nil {
			return err
		}
		tag := agentTagPrefix + name
		if who == "mine" {
			tag = cfg.Tags[0]
		}
		inv, err := inviteWithKey(ctx, api, cfg, name, tag)
		if err != nil {
			return err
		}
		printInvite(os.Stdout, inv)
	}
}

func printInvite(w io.Writer, inv Invite) {
	fmt.Fprintln(w, boxStyle.Render(inv.Encode()))
	fmt.Fprintf(w, "Send this privately to %s: it holds a single-use key valid 7 days.\n", inv.Name)
	fmt.Fprintln(w, dimStyle.Render("On their machine: install agent-relay, run `agent-relay init` and choose \"I have an invite\"."))
}

// Non-interactive

func initNonInteractive(ctx context.Context, a *initAnswers, prev *Config) error {
	switch a.Mode {
	case "join":
		raw := os.Getenv("AGENT_RELAY_INVITE")
		if raw == "" {
			data, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
			if err != nil {
				return err
			}
			raw = string(data)
		}
		inv, err := DecodeInvite(raw)
		if err != nil {
			return err
		}
		a.Name = inv.Name
		return applyJoin(ctx, a, inv, prev)
	case "host":
		if a.Token == "" {
			return errors.New("--host needs an API access token in $TS_API_KEY")
		}
		api := NewTSAPI(a.Token)
		devices, err := api.Devices(ctx)
		if err != nil {
			return err
		}
		a.CloseOpen = isDedicated(devices)
		if !a.CloseOpen {
			cur, _, err := api.Policy(ctx)
			if err != nil {
				return err
			}
			if plan, err := planPolicy(cur, nil, false); err != nil || plan.OpenRules > 0 {
				return errors.New("this tailnet has other devices and an allow-all rule; run init interactively or use a dedicated tailnet")
			}
		}
		return applyHost(ctx, a, api, devices, prev, false)
	}
	return errors.New("--yes needs --host or --join")
}

// Uninstall

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	_ = fs.Parse(args)
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("nothing to uninstall: %w", err)
	}
	if mcpUp(cfg.MCPListen) {
		return fmt.Errorf("a relay is running on %s; end that session first", cfg.MCPListen)
	}
	if !*yes {
		ok := false
		if err := huh.NewConfirm().Title("Remove agent-relay from this machine?").
			Description("Detaches the relay from your agents and deletes its tailnet identity and messages.\n" +
				"Your workspace folder stays.").Value(&ok).Run(); err != nil || !ok {
			return err
		}
	}
	prog := &Progress{echo: true}
	unwireClients(cfg, prog)
	if tok := loadAPIToken(); tok != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		api := NewTSAPI(tok)
		devices, err := api.Devices(ctx)
		if err != nil {
			return err
		}
		for _, d := range devices {
			if d.ShortName() == cfg.Hostname {
				if err := api.DeleteDevice(ctx, d.ID); err != nil {
					return err
				}
				prog.Add("removed %s from the tailnet", cfg.Hostname)
			}
		}
	} else {
		prog.Add("ask the network owner to remove %s from their tailnet", cfg.Hostname)
	}
	if err := os.RemoveAll(cfg.StateDir); err != nil {
		return err
	}
	cfgPath, _ := configPath()
	tokPath, _ := apiTokenPath()
	for _, p := range []string{cfgPath, tokPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	prog.Add("deleted the relay's identity, messages and config")
	return nil
}
