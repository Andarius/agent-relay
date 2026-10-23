package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parsePeer reads NAME=IDENTITY[,host=HOST] where IDENTITY is tag:… or a Tailscale login.
func parsePeer(v string) (string, Peer, error) {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || rest == "" {
		return "", Peer{}, fmt.Errorf("peer %q: want NAME=tag:…|login[,host=HOST]", v)
	}
	parts := strings.Split(rest, ",")
	var p Peer
	if strings.HasPrefix(parts[0], "tag:") {
		p.Tag = parts[0]
	} else {
		p.Login = parts[0]
	}
	for _, extra := range parts[1:] {
		h, ok := strings.CutPrefix(extra, "host=")
		if !ok {
			return "", Peer{}, fmt.Errorf("peer %q: unknown option %q", v, extra)
		}
		p.Host = h
	}
	return name, p, nil
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cli := fs.Bool("cli", false, "configure from flags instead of opening the setup page")
	noOpen := fs.Bool("no-open", false, "print the setup page URL without opening a browser")
	name := fs.String("name", "", "this machine's agent name")
	workspace := fs.String("workspace", "", "agent workspace folder")
	sandbox := fs.String("sandbox", "", "sbx sandbox to create or wire (empty: none)")
	container := fs.String("container", "", "container sandbox name, run with docker or podman (empty: none)")
	invite := fs.String("invite", "", "invite from a friend (fills name, tags and peer)")
	newNode := fs.Bool("new-node", false, "start a new tailnet identity (moves the old one aside)")
	var tags, peers listFlag
	fs.Var(&tags, "tag", "tag this relay advertises, e.g. tag:agent-alice; repeatable")
	fs.Var(&peers, "peer", "NAME=tag:…|login[,host=HOST]; repeatable")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "agent-relay setup [--cli flags]. A tailnet auth key, if any, comes from $TS_AUTHKEY.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if !*cli {
		return runSetupGUI(!*noOpen)
	}

	req := SetupRequest{Name: *name, Workspace: *workspace, Sandbox: *sandbox, Tags: tags, Peers: map[string]Peer{},
		AuthKey: os.Getenv("TS_AUTHKEY"), NewNode: *newNode}
	if *invite != "" {
		inv, err := DecodeInvite(*invite)
		if err != nil {
			return err
		}
		req = RequestFromInvite(inv, *workspace, *sandbox)
		req.NewNode = req.NewNode || *newNode
	}
	req.Container = *container
	for _, p := range peers {
		n, peer, err := parsePeer(p)
		if err != nil {
			return err
		}
		req.Peers[n] = peer
	}
	prog := &Progress{echo: true}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cfg, run, err := ApplySetup(ctx, req, prog)
	if err != nil {
		return err
	}
	defer run.Close()
	if err := FinishSetup(ctx, cfg, run, prog); err != nil {
		return err
	}
	if snippet := policySnippet(cfg); snippet != "" {
		fmt.Fprintf(os.Stderr, "\nTailnet policy entries for tagged relays (add once in the admin console):\n%s\n", snippet)
	}
	fmt.Fprintln(os.Stderr, "\nDone. Start agent sessions with: agent-relay run")
	return nil
}

func cmdInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	id := fs.String("id", "", "their identity: a tag (default tag:agent-NAME) or a Tailscale login")
	mine := fs.Bool("mine", false, "NAME is another machine of yours: it gets your own tag")
	host := fs.String("host", "", "their relay hostname (default relay-NAME)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "agent-relay invite NAME [--mine] [--id tag:…|LOGIN] [--host HOST]")
		fmt.Fprintln(os.Stderr, "With an API token (saved by init, or $TS_API_KEY) the invite gets a fresh single-use key;")
		fmt.Fprintln(os.Stderr, "otherwise put one, tagged with their tag, in $FRIEND_AUTHKEY.")
		fs.PrintDefaults()
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return fmt.Errorf("missing NAME")
	}
	friend := args[0]
	_ = fs.Parse(args[1:])
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	switch {
	case *mine && len(cfg.Tags) == 0:
		return fmt.Errorf("--mine needs this relay to have a tag; run agent-relay init first")
	case *mine:
		*id = cfg.Tags[0]
	case *id == "":
		*id = agentTagPrefix + friend
	}
	p := Peer{Host: *host}
	if strings.HasPrefix(*id, "tag:") {
		p.Tag = *id
	} else {
		p.Login = *id
	}
	var inv Invite
	if tok := loadAPIToken(); tok != "" && os.Getenv("FRIEND_AUTHKEY") == "" && p.Tag != "" && p.Host == "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		inv, err = inviteWithKey(ctx, NewTSAPI(tok), cfg, friend, p.Tag)
	} else {
		inv, err = MakeInvite(cfg, p, friend, os.Getenv("FRIEND_AUTHKEY"))
	}
	if err != nil {
		return err
	}
	fmt.Println(inv.Encode())
	fmt.Fprintf(os.Stderr, "\n%s added as a peer. Send the line above privately", friend)
	if inv.AuthKey != "" {
		fmt.Fprint(os.Stderr, " (it contains a tailnet auth key)")
	}
	fmt.Fprintln(os.Stderr, ". Restart running relay sessions to pick up the new peer.")
	if inv.AuthKey == "" {
		if snippet := policySnippet(cfg); snippet != "" {
			fmt.Fprintf(os.Stderr, "\nTailnet policy entries (admin console):\n%s\n", snippet)
		}
	}
	return nil
}
