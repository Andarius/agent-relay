package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// agentComms maps the process name of an agent's long-lived process to its client; vibe's is the launcher
// because vibe-app-server gets a stripped environment.
var agentComms = map[string]string{"codex": "codex", "vibe-rs": "vibe", "claude": "claude"}

var errNoAgent = errors.New("not running under codex, vibe or claude")

type procInfo struct {
	comm, start string
	ppid        int
}

// readProc parses /proc/<pid>/stat; comm sits in parentheses and may itself hold spaces or parentheses.
func readProc(proc string, pid int) (procInfo, error) {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procInfo{}, err
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return procInfo{}, errors.New("bad stat for pid " + strconv.Itoa(pid))
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 {
		return procInfo{}, errors.New("short stat for pid " + strconv.Itoa(pid))
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procInfo{}, err
	}
	return procInfo{comm: string(b[open+1 : end]), ppid: ppid, start: f[19]}, nil
}

// procAlive reports whether pid still runs the process that started at start (pids get reused).
func procAlive(proc string, pid int, start string) bool {
	p, err := readProc(proc, pid)
	return err == nil && p.start == start
}

// findAgent walks up from pid to the nearest agent process and returns it with its launch environment,
// which its MCP servers and hooks do not inherit.
func findAgent(proc string, pid int) (LocalAgent, map[string]string, error) {
	for pid > 1 {
		p, err := readProc(proc, pid)
		if err != nil {
			return LocalAgent{}, nil, err
		}
		if client, ok := agentComms[p.comm]; ok {
			env := readEnviron(filepath.Join(proc, strconv.Itoa(pid), "environ"))
			cwd, _ := os.Readlink(filepath.Join(proc, strconv.Itoa(pid), "cwd"))
			a := LocalAgent{Client: client, PID: pid, ProcStart: p.start, Cwd: cwd, TmuxPane: env["TMUX_PANE"]}
			if sock, _, ok := strings.Cut(env["TMUX"], ","); ok {
				a.TmuxSock = sock
			}
			a.Name = agentName(env["AGENT_RELAY_NAME"], client, cwd)
			return a, env, nil
		}
		pid = p.ppid
	}
	return LocalAgent{}, nil, errNoAgent
}

func readEnviron(path string) map[string]string {
	env := map[string]string{}
	b, _ := os.ReadFile(path)
	for kv := range strings.SplitSeq(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

// agentName is $AGENT_RELAY_NAME when set, else <client>-<folder>, cut down to a valid peer name.
func agentName(explicit, client, cwd string) string {
	raw := explicit
	if raw == "" {
		raw = client + "-" + filepath.Base(cwd)
	}
	n := strings.Trim(nonName.ReplaceAllString(strings.ToLower(raw), "-"), "-")
	if len(n) > 36 {
		n = strings.TrimRight(n[:36], "-")
	}
	if n == "" {
		return client
	}
	return n
}
