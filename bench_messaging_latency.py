# /// script
# requires-python = ">=3.14"
# dependencies = []
# ///
# Per-phase latency of agent-relay messaging: local mode (stdio MCP + SQLite + tmux nudge / hooks) and,
# when a relay runs on 127.0.0.1:8770, remote mode (HTTP MCP over the tailnet + doorbell).
# Env: AGENT_RELAY_BIN (default: agent-relay on PATH), BENCH_N (default 20), BENCH_PEER (remote peer name),
# BENCH_TOKEN_FILE (default ~/.local/share/agent-relay/relay-spatchou/mcp-token).
import json
import os
import shutil
import sqlite3
import statistics
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path

BIN = os.environ.get("AGENT_RELAY_BIN") or shutil.which("agent-relay") or "agent-relay"
N = int(os.environ.get("BENCH_N", "20"))
PEER = os.environ.get("BENCH_PEER", "linux-mini")
TOKEN_FILE = Path(os.environ.get("BENCH_TOKEN_FILE", "~/.local/share/agent-relay/relay-spatchou/mcp-token")).expanduser()
RESULTS: dict[str, list[float]] = {}


def record(phase: str, seconds: float) -> None:
    RESULTS.setdefault(phase, []).append(seconds * 1000)


class StdioMCP:
    """Minimal newline-delimited JSON-RPC client for an MCP server on stdio."""

    def __init__(self, argv: list[str], env: dict[str, str]):
        self.p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, env=env, text=True, bufsize=1)
        self.next_id = 0

    def _send(self, msg: dict) -> None:
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()

    def call(self, method: str, params: dict | None = None) -> dict:
        self.next_id += 1
        self._send({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params or {}})
        while True:
            msg = json.loads(self.p.stdout.readline())
            if msg.get("id") == self.next_id:
                return msg

    def init(self) -> None:
        self.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "bench", "version": "0"}})
        self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def tool(self, name: str, args: dict) -> dict:
        r = self.call("tools/call", {"name": name, "arguments": args})
        if "error" in r or r["result"].get("isError"):
            raise RuntimeError(f"{name}: {r}")
        return r["result"]

    def close(self) -> None:
        self.p.stdin.close()
        self.p.wait(timeout=5)


def tmux(*args: str) -> str:
    return subprocess.run(["tmux", "-L", "arbench", *args], check=True, capture_output=True, text=True).stdout.strip()


def bench_local(tmp: Path) -> None:
    codex = tmp / "codex"  # a process named codex makes agent-relay treat it as the agent
    codex.symlink_to(shutil.which("bash"))
    base = {"PATH": os.environ["PATH"], "HOME": os.environ["HOME"], "AGENT_RELAY_HOME": str(tmp)}

    def agent(name: str, cmd: str, extra: dict | None = None) -> list[str] | dict:
        return [str(codex), "-c", f"{cmd}; true"], {**base, "AGENT_RELAY_NAME": name, **(extra or {})}

    stamps = tmp / "pane-stamps"
    reader = tmp / "reader.py"  # stands in for the agent's prompt: stamps each submitted line
    reader.write_text(f"import sys, time\nwhile sys.stdin.readline():\n    open({str(stamps)!r}, 'a').write(f'{{time.time()}}\\n')\n")
    tmux("new-session", "-d", "-s", "b", "-x", "200", "-y", "20", f"python3 -u {reader}")
    pane, sock = tmux("display", "-p", "-t", "b", "#{pane_id}"), tmux("display", "-p", "#{socket_path}")

    for i in range(N):
        argv, env = agent(f"cold{i}", f'"{BIN}" mcp')
        t0 = time.perf_counter()
        m = StdioMCP(argv, env)
        m.init()
        record("local  MCP server cold start (spawn + initialize)", time.perf_counter() - t0)
        m.close()

    sender, recv = (StdioMCP(*agent(n, f'"{BIN}" mcp', x)) for n, x in
                    [("send", None), ("recv", {"TMUX_PANE": pane, "TMUX": f"{sock},0,0"})])
    sender.init()
    recv.init()
    db = sqlite3.connect(tmp / "state" / "local.db", isolation_level=None)
    set_state = lambda s: db.execute("UPDATE agents SET state = ? WHERE name = 'recv'", (s,))

    for i in range(N):
        set_state("busy")
        t0 = time.perf_counter()
        sender.tool("send_message", {"to": "recv", "body": f"busy {i}"})
        record("local  send_message, recipient busy (store only)", time.perf_counter() - t0)
        t0 = time.perf_counter()
        recv.tool("read_inbox", {})
        record("local  read_inbox (1 message)", time.perf_counter() - t0)

    for i in range(N):
        set_state("idle")
        before = stamps.read_text().count("\n") if stamps.exists() else 0
        wall0, t0 = time.time(), time.perf_counter()
        sender.tool("send_message", {"to": "recv", "body": f"idle {i}"})
        record("local  send_message, recipient idle (store + tmux nudge)", time.perf_counter() - t0)
        while not stamps.exists() or stamps.read_text().count("\n") == before:
            time.sleep(0.002)
        record("local  wake: send -> nudge submitted in recipient pane", float(stamps.read_text().split()[-1]) - wall0)
        recv.tool("read_inbox", {})

    hook_input = tmp / "stop.json"
    hook_input.write_text('{"hook_event_name":"Stop"}')
    for i in range(N):
        argv, env = agent(f"base{i}", "true")
        t0 = time.perf_counter()
        subprocess.run(argv, env=env, check=True)
        record("local  (baseline) spawn fake agent shell", time.perf_counter() - t0)

        argv, env = agent(f"hk{i}", f'"{BIN}" hook < {hook_input}')
        t0 = time.perf_counter()
        out = subprocess.run(argv, env=env, check=True, capture_output=True, text=True).stdout
        record("local  Stop hook, nothing pending (mark idle)", time.perf_counter() - t0)

        db.execute("INSERT INTO local_messages (id, sender, recipient, body, created_at) VALUES (?, 'x', ?, 'b', 't')",
                   (f"m{i}", f"hk{i}"))
        t0 = time.perf_counter()
        out = subprocess.run(argv, env=env, check=True, capture_output=True, text=True).stdout
        record("local  Stop hook, message pending (block turn end)", time.perf_counter() - t0)
        if '"decision":"block"' not in out:
            raise RuntimeError(f"hook did not block: {out!r}")

    for m in (sender, recv):
        m.close()
    tmux("kill-server")


def http_mcp(token: str, name: str, args: dict) -> dict:
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": name, "arguments": args}})
    req = urllib.request.Request(f"http://127.0.0.1:8770/mcp/{token}", data=body.encode(), method="POST",
                                 headers={"Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        r = json.loads(resp.read())
    if "error" in r or r["result"].get("isError"):
        raise RuntimeError(f"{name}: {r}")
    return r["result"]


def bench_remote(tmp: Path) -> None:
    token = TOKEN_FILE.read_text().strip()
    peers = http_mcp(token, "list_peers", {})["structuredContent"]["peers"]
    if not any(p["name"] == PEER and p["reachable"] for p in peers):
        raise RuntimeError(f"peer {PEER} unreachable: {peers}")
    for i in range(min(N, 25)):  # the peer accepts 30 messages per minute per sender
        t0 = time.perf_counter()
        http_mcp(token, "send_message", {"to": PEER, "body": f"bench {i}"})
        record(f"remote send_message -> {PEER} (MCP HTTP + tailnet + peer store)", time.perf_counter() - t0)
        t0 = time.perf_counter()
        http_mcp(token, "read_inbox", {})
        record("remote read_inbox (local MCP HTTP)", time.perf_counter() - t0)

    inbox = tmp / "inbox"
    inbox.mkdir()
    # the doorbell loop agent-relay installs: poll the inbox dir every 2 s
    bell = subprocess.Popen(["bash", "-c", f'while true; do for f in {inbox}/*.json; do [ -e "$f" ] && echo "$f" && rm -f "$f"; done; sleep 2; done'],
                            stdout=subprocess.PIPE, text=True, bufsize=1)
    for i in range(min(N, 10)):
        t0 = time.perf_counter()
        (inbox / f"{i}.json").write_text("{}")
        bell.stdout.readline()
        record("remote wake: doorbell file -> Monitor line (2 s poll)", time.perf_counter() - t0)
        time.sleep(0.37 * (i % 3))  # spread arrivals over the poll cycle
    bell.kill()


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="arbench-") as d:
        tmp = Path(d)
        bench_local(tmp)
        try:
            bench_remote(tmp)
        except Exception as e:
            print(f"remote skipped: {e}")
    print(f"\n{'phase':<70} {'median':>9} {'p90':>9} {'min':>9} {'n':>4}")
    for phase, ms in RESULTS.items():
        q = statistics.quantiles(ms, n=10) if len(ms) > 1 else [ms[0]] * 9
        print(f"{phase:<70} {statistics.median(ms):>7.1f}ms {q[8]:>7.1f}ms {min(ms):>7.1f}ms {len(ms):>4}")
    print(f"{'native Claude Code SendMessage':<70} {'not measurable from outside Claude Code':>36}")


if __name__ == "__main__":
    main()
