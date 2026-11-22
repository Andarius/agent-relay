package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type fakeProc struct {
	pid, ppid   int
	comm, start string
	env         []string
	cwd         string
}

// newProcfs writes a minimal /proc with stat, environ and cwd for each process.
func newProcfs(t *testing.T, procs ...fakeProc) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) S %d %s%s 0 0\n", p.pid, p.comm, p.ppid, strings.Repeat("0 ", 17), p.start)
		must(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600))
		must(t, os.WriteFile(filepath.Join(dir, "environ"), []byte(strings.Join(p.env, "\x00")), 0o600))
		if p.cwd != "" {
			must(t, os.Symlink(p.cwd, filepath.Join(dir, "cwd")))
		}
	}
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFindAgent(t *testing.T) {
	tests := []struct {
		name    string
		procs   []fakeProc
		want    LocalAgent
		wantErr error
	}{
		{
			name: "codex with an explicit name and tmux",
			procs: []fakeProc{
				{pid: 10, ppid: 1, comm: "codex", start: "500", cwd: "/w/api",
					env: []string{"AGENT_RELAY_NAME=Codex API", "TMUX_PANE=%3", "TMUX=/tmp/tmux-1000/default,42,0"}},
				{pid: 20, ppid: 10, comm: "agent-relay", start: "600"},
			},
			want: LocalAgent{Name: "codex-api", Client: "codex", PID: 10, ProcStart: "500", Cwd: "/w/api",
				TmuxPane: "%3", TmuxSock: "/tmp/tmux-1000/default"},
		},
		{
			name: "vibe env lives on the launcher above vibe-app-server",
			procs: []fakeProc{
				{pid: 10, ppid: 1, comm: "vibe-rs", start: "500", cwd: "/w/My Front", env: []string{"TMUX_PANE=%4"}},
				{pid: 11, ppid: 10, comm: "vibe-app-server", start: "501", cwd: "/w/My Front"},
				{pid: 12, ppid: 11, comm: "sh", start: "502"},
			},
			want: LocalAgent{Name: "vibe-my-front", Client: "vibe", PID: 10, ProcStart: "500", Cwd: "/w/My Front", TmuxPane: "%4"},
		},
		{
			name: "comm with spaces and parentheses",
			procs: []fakeProc{
				{pid: 10, ppid: 1, comm: "claude", start: "500", cwd: "/w/x"},
				{pid: 11, ppid: 10, comm: "a (b) c", start: "501"},
			},
			want: LocalAgent{Name: "claude-x", Client: "claude", PID: 10, ProcStart: "500", Cwd: "/w/x"},
		},
		{
			name:    "no agent above",
			procs:   []fakeProc{{pid: 10, ppid: 1, comm: "zsh", start: "500"}},
			wantErr: errNoAgent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := newProcfs(t, tt.procs...)
			got, _, err := findAgent(proc, tt.procs[len(tt.procs)-1].pid)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// localEnv gives two live agents (codex 10, vibe 20) and a dead one, each with an MCP server child.
func localEnv(t *testing.T) (proc string, store *LocalStore) {
	t.Helper()
	t.Setenv("AGENT_RELAY_HOME", t.TempDir())
	proc = newProcfs(t,
		fakeProc{pid: 10, ppid: 1, comm: "codex", start: "100", cwd: "/w/api", env: []string{"AGENT_RELAY_NAME=api", "TMUX_PANE=%6"}},
		fakeProc{pid: 11, ppid: 10, comm: "agent-relay", start: "101"},
		fakeProc{pid: 20, ppid: 1, comm: "vibe-rs", start: "200", cwd: "/w/front", env: []string{"AGENT_RELAY_NAME=front", "TMUX_PANE=%7"}},
		fakeProc{pid: 21, ppid: 20, comm: "agent-relay", start: "201"},
	)
	path, err := localStatePath(os.Getenv)
	must(t, err)
	store, err = OpenLocalStore(path, proc)
	must(t, err)
	t.Cleanup(func() { store.Close() })
	return proc, store
}

func session(t *testing.T, proc string, pid int) *localSession {
	t.Helper()
	ls, err := openLocalSession(context.Background(), proc, pid)
	must(t, err)
	t.Cleanup(func() { ls.store.Close() })
	ls.nudge = func(LocalAgent, string) error { return nil } // tests never drive a real tmux
	return ls
}

func TestRegisterNames(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		held  LocalAgent // already in the store
		agent LocalAgent
		want  string
	}{
		{"free name", LocalAgent{}, LocalAgent{Name: "api", PID: 10, ProcStart: "100"}, "api"},
		{"held by a live process", LocalAgent{Name: "api", PID: 20, ProcStart: "200"},
			LocalAgent{Name: "api", PID: 10, ProcStart: "100"}, "api-2"},
		{"held by a dead process", LocalAgent{Name: "api", PID: 99, ProcStart: "1"},
			LocalAgent{Name: "api", PID: 10, ProcStart: "100"}, "api"},
		{"pid reused by another process", LocalAgent{Name: "api", PID: 20, ProcStart: "150"},
			LocalAgent{Name: "api", PID: 10, ProcStart: "100"}, "api"},
		{"same process registers again", LocalAgent{Name: "api", PID: 10, ProcStart: "100"},
			LocalAgent{Name: "api", PID: 10, ProcStart: "100"}, "api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, store := localEnv(t)
			if tt.held.Name != "" {
				_, err := store.Register(ctx, tt.held)
				must(t, err)
			}
			got, err := store.Register(ctx, tt.agent)
			must(t, err)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSendValidation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		in      LocalSendIn
		wantErr string
	}{
		{"delivered", LocalSendIn{To: "front", Body: "hi"}, ""},
		{"with a card", LocalSendIn{To: "front", Body: "hi", Card: msgID}, ""},
		{"unknown agent", LocalSendIn{To: "ghost", Body: "hi"}, `no running agent "ghost"`},
		{"agent that exited", LocalSendIn{To: "gone", Body: "hi"}, `no running agent "gone"`},
		{"to self", LocalSendIn{To: "api", Body: "hi"}, "that is you"},
		{"empty body", LocalSendIn{To: "front", Body: "  "}, "body must be"},
		{"oversized body", LocalSendIn{To: "front", Body: strings.Repeat("x", maxBody+1)}, "body must be"},
		{"bad card", LocalSendIn{To: "front", Body: "hi", Card: "x"}, "card must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, store := localEnv(t)
			api := session(t, proc, 11)
			session(t, proc, 21)
			_, err := store.Register(ctx, LocalAgent{Name: "gone", PID: 99, ProcStart: "1"})
			must(t, err)
			_, out, err := api.send(ctx, nil, tt.in)
			if tt.wantErr == "" {
				must(t, err)
				if out.Status != "delivered" {
					t.Fatalf("status %q", out.Status)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestWakeFlow walks messages through both wake paths: the end-of-turn hook and the sender's tmux nudge.
func TestWakeFlow(t *testing.T) {
	ctx := context.Background()
	hook := func(t *testing.T, proc string, pid int, event string) *hookBlock {
		t.Helper()
		out, err := runHook(ctx, proc, pid, strings.NewReader(`{"hook_event_name":"`+event+`"}`))
		must(t, err)
		return out
	}
	tests := []struct {
		name                      string
		recvPID, sendPID          int // processes under the receiving and sending agents
		recvName, sendName        string
		start, startTurn, endTurn string
		decision                  string
	}{
		{"vibe receiver", 21, 11, "front", "api", "session_start", "pre_agent", "post_agent", "deny"},
		{"codex receiver", 11, 21, "api", "front", "SessionStart", "UserPromptSubmit", "Stop", "block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, _ := localEnv(t)
			hook(t, proc, tt.recvPID, tt.start) // the receiver's MCP server never started: hooks register it
			sender := session(t, proc, tt.sendPID)
			var nudges []string
			sender.nudge = func(to LocalAgent, text string) error {
				nudges = append(nudges, to.Name+": "+text)
				return nil
			}
			send := func(body string) {
				t.Helper()
				_, _, err := sender.send(ctx, nil, LocalSendIn{To: tt.recvName, Body: body})
				must(t, err)
			}

			// mid-turn: no nudge, the end-of-turn hook keeps the receiver going once
			hook(t, proc, tt.recvPID, tt.startTurn)
			send("first")
			want := &hookBlock{Decision: tt.decision, Reason: "agent-relay: 1 new message(s) from " + tt.sendName + ". Call read_inbox."}
			if got := hook(t, proc, tt.recvPID, tt.endTurn); got == nil || *got != *want {
				t.Fatalf("end of turn = %+v, want %+v", got, want)
			}
			if got := hook(t, proc, tt.recvPID, tt.endTurn); got != nil {
				t.Fatalf("same message woke the receiver twice: %+v", got)
			}
			if len(nudges) != 0 {
				t.Fatalf("nudged a busy receiver: %q", nudges)
			}

			// idle: the next message is nudged into its pane, a second one while it works is not
			send("second")
			send("third")
			wantNudge := tt.recvName + ": agent-relay: 1 new message(s) from " + tt.sendName + ". Call read_inbox."
			if len(nudges) != 1 || nudges[0] != wantNudge {
				t.Fatalf("nudges = %q, want [%q]", nudges, wantNudge)
			}

			recv := session(t, proc, tt.recvPID)
			_, inbox, err := recv.readInbox(ctx, nil, InboxIn{})
			must(t, err)
			var bodies []string
			for _, m := range inbox.Messages {
				bodies = append(bodies, m.From+":"+m.Body)
			}
			if got := strings.Join(bodies, ","); got != tt.sendName+":first,"+tt.sendName+":second,"+tt.sendName+":third" {
				t.Fatalf("inbox = %s", got)
			}
			if _, again, _ := recv.readInbox(ctx, nil, InboxIn{}); len(again.Messages) != 0 {
				t.Fatalf("messages not marked read: %+v", again.Messages)
			}
		})
	}
}

func TestWakeNudgeResult(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		nudgeErr  error
		wantState string
		wantBlock bool // the receiver's next end of turn still announces the message
	}{
		{"nudge typed", nil, stateBusy, false},
		{"nudge lost: claim undone", errNotTyped, stateIdle, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc, store := localEnv(t)
			sender, recv := session(t, proc, 11), session(t, proc, 21)
			sender.nudge = func(LocalAgent, string) error { return tt.nudgeErr }
			_, _, err := sender.send(ctx, nil, LocalSendIn{To: recv.me.Name, Body: "hi"})
			must(t, err)
			live, err := store.Live(ctx)
			must(t, err)
			if got := live[slices.IndexFunc(live, func(a LocalAgent) bool { return a.Name == "front" })].State; got != tt.wantState {
				t.Fatalf("state = %q, want %q", got, tt.wantState)
			}
			out, err := runHook(ctx, proc, 21, strings.NewReader(`{"hook_event_name":"post_agent"}`))
			must(t, err)
			if (out != nil) != tt.wantBlock {
				t.Fatalf("end of turn = %+v, want block %v", out, tt.wantBlock)
			}
		})
	}
}

func TestHookOutsideAgent(t *testing.T) {
	proc := newProcfs(t, fakeProc{pid: 10, ppid: 1, comm: "zsh", start: "1"}, fakeProc{pid: 11, ppid: 10, comm: "agent-relay", start: "2"})
	t.Setenv("AGENT_RELAY_HOME", t.TempDir())
	_, err := runHook(context.Background(), proc, 11, strings.NewReader(`{"hook_event_name":"Stop"}`))
	if !errors.Is(err, errNoAgent) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetJSONHooks(t *testing.T) {
	const cmd = "/usr/bin/agent-relay hook"
	codexOther := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"tramecli hook"}]}]},"x":1}`
	claudeOther := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"tramecli hook"}]}]},"x":1,"permissions":{"allow":["Bash(ls)"]}}`
	tests := []struct {
		name, in, cmd string
		allow         []string
		wantRelay     int // agent-relay hook groups after the edit
		wantOther     bool
	}{
		{"codex: install into an empty file", "", cmd, nil, 3, false},
		{"codex: install keeps other hooks", codexOther, cmd, nil, 3, true},
		{"codex: uninstall keeps other hooks", codexOther, "", nil, 0, true},
		{"claude: install adds allow rules, keeps others", claudeOther, cmd, claudeAllow(), 3, true},
		{"claude: uninstall keeps other hooks and rules", claudeOther, "", claudeAllow(), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			once, err := setJSONHooks([]byte(tt.in), tt.cmd, tt.allow)
			must(t, err)
			twice, err := setJSONHooks(once, tt.cmd, tt.allow)
			must(t, err)
			if string(once) != string(twice) {
				t.Fatalf("not idempotent:\n%s\n%s", once, twice)
			}
			if got := strings.Count(string(once), cmd); got != tt.wantRelay {
				t.Fatalf("relay hooks = %d, want %d:\n%s", got, tt.wantRelay, once)
			}
			if got := strings.Contains(string(once), "tramecli hook") && strings.Contains(string(once), `"x": 1`); got != tt.wantOther {
				t.Fatalf("other content kept = %v:\n%s", got, once)
			}
			for _, rule := range tt.allow {
				if got := strings.Contains(string(once), rule); got != (tt.cmd != "") {
					t.Fatalf("allow rule %s present = %v:\n%s", rule, got, once)
				}
			}
			if tt.allow != nil && !strings.Contains(string(once), "Bash(ls)") {
				t.Fatalf("dropped another allow rule:\n%s", once)
			}
			if uninstalled, _ := setJSONHooks(once, "", tt.allow); strings.Contains(string(uninstalled), cmd) {
				t.Fatalf("uninstall left relay hooks:\n%s", uninstalled)
			}
		})
	}
}

func TestSetVibeHooks(t *testing.T) {
	const cmd = "/usr/bin/agent-relay hook"
	other := "[[hooks]]\nname = \"mine\"\ntype = \"post_tool\"\ncommand = \"true\""
	tests := []struct {
		name, in string
	}{
		{"empty file", ""},
		{"other hooks without a trailing newline", other},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			once := setVibeHooks([]byte(tt.in), cmd)
			if twice := setVibeHooks(once, cmd); string(once) != string(twice) {
				t.Fatalf("not idempotent:\n%s\n%s", once, twice)
			}
			if !strings.HasPrefix(string(once), tt.in) || strings.Count(string(once), `"`+cmd+`"`) != 3 {
				t.Fatalf("install:\n%s", once)
			}
			if back := strings.TrimRight(string(setVibeHooks(once, "")), "\n"); back != tt.in {
				t.Fatalf("uninstall = %q, want %q", back, tt.in)
			}
		})
	}
}

func TestSetTomlTable(t *testing.T) {
	const h = "mcp_servers.agent-relay-local"
	body := []string{`command = "/bin/agent-relay"`, `args = ["mcp"]`}
	tests := []struct {
		name, in string
		body     []string
		want     string
	}{
		{"append to empty", "", body, "[" + h + "]\ncommand = \"/bin/agent-relay\"\nargs = [\"mcp\"]\n"},
		{"append after other tables", "model = \"x\"\n\n[features]\na = true\n", body,
			"model = \"x\"\n\n[features]\na = true\n\n[" + h + "]\ncommand = \"/bin/agent-relay\"\nargs = [\"mcp\"]\n"},
		{"replace table and its subtables, keep neighbours", "[a]\nx = 1\n\n[" + h + "]\ncommand = \"old\"\n\n[" + h + ".env]\nK = \"v\"\n\n[b]\ny = 2\n",
			body, "[a]\nx = 1\n\n[b]\ny = 2\n\n[" + h + "]\ncommand = \"/bin/agent-relay\"\nargs = [\"mcp\"]\n"},
		{"similar name is not touched", "[" + h + "-2]\nz = 3\n", nil, "[" + h + "-2]\nz = 3\n"},
		{"remove", "[a]\nx = 1\n\n[" + h + "]\ncommand = \"old\"\n", nil, "[a]\nx = 1\n"},
		{"remove the only table", "[" + h + "]\ncommand = \"old\"\n", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(setTomlTable([]byte(tt.in), h, tt.body))
			if got != tt.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tt.want)
			}
			if again := string(setTomlTable([]byte(got), h, tt.body)); again != got {
				t.Fatalf("not idempotent:\n%s", again)
			}
		})
	}
}
