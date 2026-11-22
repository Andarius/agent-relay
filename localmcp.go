package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// localSession is the MCP server one agent session runs: it knows who it speaks for from the process tree.
type localSession struct {
	store *LocalStore
	me    LocalAgent
	nudge func(a LocalAgent, text string) error
}

type ListAgentsOut struct {
	You    string       `json:"you"`
	Agents []LocalAgent `json:"agents"`
}

type LocalSendIn struct {
	To   string `json:"to" jsonschema:"agent name from list_agents"`
	Body string `json:"body" jsonschema:"message text"`
	Card string `json:"card,omitempty" jsonschema:"id of the Trame card the message is about, if any"`
}

// cmdMCP serves the local MCP server on stdio for the agent session that launched it.
func cmdMCP(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("mcp takes no arguments, got %q", args)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ls, err := openLocalSession(ctx, "/proc", os.Getppid())
	if err != nil {
		return err
	}
	defer ls.store.Close()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return ls.server().Run(ctx, &mcp.StdioTransport{})
}

// openLocalSession finds the agent above pid and registers it under the name it holds or a new one.
func openLocalSession(ctx context.Context, proc string, pid int) (*localSession, error) {
	me, env, err := findAgent(proc, pid)
	if err != nil {
		return nil, err
	}
	path, err := localStatePath(func(k string) string { return cmp.Or(os.Getenv(k), env[k]) })
	if err != nil {
		return nil, err
	}
	store, err := OpenLocalStore(path, proc)
	if err != nil {
		return nil, err
	}
	if me.Name, err = store.Register(ctx, me); err != nil {
		store.Close()
		return nil, err
	}
	return &localSession{store: store, me: me, nudge: tmuxNudge}, nil
}

func (ls *localSession) server() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "agent-relay-local", Version: version}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("You are %q. Talk to the other agent sessions on this machine. %s", ls.me.Name, untrustedMsg),
	})
	mcp.AddTool(srv, &mcp.Tool{Name: "list_agents", Description: "List the agent sessions running on this machine."}, ls.listAgents)
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a text message to another agent session on this machine."}, ls.send)
	mcp.AddTool(srv, &mcp.Tool{Name: "read_inbox", Description: "Return received messages, oldest first, and mark them read."}, ls.readInbox)
	return srv
}

func (ls *localSession) listAgents(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListAgentsOut, error) {
	live, err := ls.store.Live(ctx)
	return nil, ListAgentsOut{You: ls.me.Name, Agents: live}, err
}

func (ls *localSession) send(ctx context.Context, _ *mcp.CallToolRequest, in LocalSendIn) (*mcp.CallToolResult, SendOut, error) {
	if strings.TrimSpace(in.Body) == "" || len(in.Body) > maxBody {
		return nil, SendOut{}, fmt.Errorf("body must be 1..%d chars", maxBody)
	}
	if !validCard(in.Card) {
		return nil, SendOut{}, errors.New("card must be a Trame card id (UUID)")
	}
	if in.To == ls.me.Name {
		return nil, SendOut{}, errors.New("that is you")
	}
	live, err := ls.store.Live(ctx)
	if err != nil {
		return nil, SendOut{}, err
	}
	i := slices.IndexFunc(live, func(a LocalAgent) bool { return a.Name == in.To })
	if i < 0 {
		names := make([]string, 0, len(live))
		for _, a := range live {
			names = append(names, a.Name)
		}
		return nil, SendOut{}, fmt.Errorf("no running agent %q; running: %s", in.To, strings.Join(names, ", "))
	}
	id := uuid.NewString()
	if err := ls.store.Send(ctx, id, ls.me.Name, in.To, in.Body, in.Card); err != nil {
		return nil, SendOut{}, err
	}
	ls.wake(ctx, live[i])
	return nil, SendOut{ID: id, Status: "delivered"}, nil
}

// wake types a prompt into the recipient's tmux pane when it waits for input; a busy recipient is woken by
// its end-of-turn hook instead.
func (ls *localSession) wake(ctx context.Context, to LocalAgent) {
	if to.TmuxPane == "" {
		return
	}
	senders, err := ls.store.Claim(ctx, to.Name, claimIfIdle)
	if err != nil {
		slog.Error("wake", "to", to.Name, "err", err)
		return
	}
	if len(senders) == 0 {
		return
	}
	if err := ls.nudge(to, wakeText(senders)); err != nil {
		slog.Error("tmux nudge", "to", to.Name, "pane", to.TmuxPane, "err", err)
		if err := ls.store.Unclaim(ctx, to.Name); err != nil {
			slog.Error("unclaim", "to", to.Name, "err", err)
		}
	}
}

func (ls *localSession) readInbox(ctx context.Context, _ *mcp.CallToolRequest, in InboxIn) (*mcp.CallToolResult, InboxOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	msgs, err := ls.store.Inbox(ctx, ls.me.Name, in.IncludeRead, min(limit, 100), in.Card)
	return nil, InboxOut{Note: untrustedMsg, Messages: msgs}, err
}

func wakeText(senders []string) string {
	slices.Sort(senders)
	return fmt.Sprintf("agent-relay: %d new message(s) from %s. Call read_inbox.", len(senders),
		strings.Join(slices.Compact(senders), ", "))
}

// submitDelay separates the text from Enter: Codex takes an Enter that arrives with the text as part of a paste.
const submitDelay = 300 * time.Millisecond

var errNotTyped = errors.New("nudge text did not reach the prompt; Enter not sent")

// tmuxNudge types text tagged with a fresh token and presses Enter only once the token shows in the pane: an
// Enter on an empty prompt would run whatever the agent suggests there (Claude's suggested prompts).
func tmuxNudge(a LocalAgent, text string) error {
	tmux := func(args ...string) (string, error) {
		if a.TmuxSock != "" {
			args = append([]string{"-S", a.TmuxSock}, args...)
		}
		out, err := exec.Command("tmux", args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	token := uuid.NewString()[:8]
	if _, err := tmux("send-keys", "-t", a.TmuxPane, "-l", text+" ("+token+")"); err != nil {
		return err
	}
	time.Sleep(submitDelay)
	screen, err := tmux("capture-pane", "-p", "-J", "-t", a.TmuxPane)
	if err != nil {
		return err
	}
	if !strings.Contains(screen, token) {
		return errNotTyped
	}
	_, err = tmux("send-keys", "-t", a.TmuxPane, "Enter")
	return err
}

type hookInput struct {
	Event string `json:"hook_event_name"`
}

type hookBlock struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// cmdHook handles an agent hook on stdin; it never fails the agent's turn, problems only go to stderr.
func cmdHook(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("hook takes no arguments, got %q", args)
	}
	out, err := runHook(context.Background(), "/proc", os.Getppid(), os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-relay hook:", err)
		return nil
	}
	if out != nil {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	return nil
}

// runHook registers the agent on any event and marks it busy when a turn starts; when a turn ends it keeps
// the agent going if messages came in meanwhile, else marks it idle so senders nudge it.
func runHook(ctx context.Context, proc string, pid int, stdin io.Reader) (*hookBlock, error) {
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fmt.Errorf("read hook input: %w", err)
	}
	ls, err := openLocalSession(ctx, proc, pid)
	if err != nil {
		return nil, err
	}
	defer ls.store.Close()
	switch in.Event {
	case "SessionStart", "session_start":
		return nil, nil
	case "UserPromptSubmit", "pre_agent":
		return nil, ls.store.SetState(ctx, ls.me.Name, stateBusy)
	case "Stop", "post_agent":
		senders, err := ls.store.Claim(ctx, ls.me.Name, claimAtTurnEnd)
		if err != nil || len(senders) == 0 {
			return nil, err
		}
		decision := "block"
		if ls.me.Client == "vibe" {
			decision = "deny"
		}
		return &hookBlock{Decision: decision, Reason: wakeText(senders)}, nil
	}
	return nil, fmt.Errorf("unhandled hook event %q", in.Event)
}
