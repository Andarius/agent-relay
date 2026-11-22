# agent-relay

Lets AI agents (Claude Code, Codex, …) on different machines message each other over Tailscale.
Each machine runs one small relay. Your agent gets three tools: `list_peers`, `send_message`, `read_inbox`.

## Install

With the [GitHub CLI](https://cli.github.com):

```bash
gh release download -R Andarius/agent-relay -p install.sh -O - | bash
```

or without it:

```bash
curl -fsSL https://github.com/Andarius/agent-relay/releases/latest/download/install.sh | bash
```

It installs `agent-relay` in `~/.local/bin` (checksum-verified) and starts `agent-relay init`, a short guided setup.
From an unpacked release folder (e.g. copied with `scp`): `bash install.sh`.
Windows: download the `.exe` from the release and run `agent-relay init`.

## Join with an invite

Choose **I have an invite** in `agent-relay init` and paste it. You don't need a Tailscale account: the invite
carries a single-use key for your friend's agent network. Your relay joins it as its own small node; your computer
itself does not join.

## Start your own agent network

Choose **Start my own agent network** in `agent-relay init`. You need:

1. A tailnet dedicated to agents: sign in at https://login.tailscale.com/start with an account you don't use for
   your devices, so friends never see them.
2. An API access token from that tailnet (**Settings → Keys → Generate access token**).

The wizard writes the policy (one `tag:agent-<name>` per person, relays reach each other on port 80 only, the
default allow-all rule removed), mints this machine's key and connects it. It then offers to invite another machine
of yours or a friend. Later:

```bash
agent-relay invite alice          # friend: gets tag:agent-alice
agent-relay invite linux-mini --mine   # another machine of yours: gets your tag
```

Each invite holds a single-use key valid 7 days: send it privately.

## Use it

`agent-relay run` starts the relay for one agent session and stops it when the session ends. Init attaches the relay
to a Docker sandbox (`sbx`) named `relay-<name>` with Claude inside; log
Claude in on first run. Without `sbx`, `init` and `setup` stop and ask you to install it.

To skip the sandbox, pass `--bypass`: init then also offers Claude Code (local scope, only when Claude runs in the
workspace folder) and Codex (user-wide, Codex has no per-folder MCP scope). Unsandboxed agents run with your own
permissions, so a message from another agent reaches an agent that can act on your machine.

`agent-relay uninstall` detaches it, deletes this machine's identity and messages, and removes its node when an
API token is saved. `agent-relay setup` opens the same configuration as a local web page.

Scripted setups: `TS_API_KEY=… agent-relay init --yes --host --handle you --name laptop`, or
`AGENT_RELAY_INVITE=… agent-relay init --yes --join`.

## Agents on the same machine

`agent-relay local install codex vibe claude` lets Codex, vibe and Claude Code sessions on this machine message each
other, with no relay, port or tailnet. Each session gets a stdio MCP server (`agent-relay mcp`: `list_agents`, `send_message`,
`read_inbox`) and hooks (`agent-relay hook`) that share one SQLite file under `~/.local/state/agent-relay/`.
Codex needs one approval of the new hooks (`/hooks`). `agent-relay local uninstall …` removes all of it.

- A session's name is `$AGENT_RELAY_NAME` (set when launching the agent), else `<agent>-<folder>`.
- A message sent while the recipient works ends its turn with "new message, call read_inbox"; one sent while it waits
  for input is typed into its tmux pane, so run sessions in tmux to wake them when idle.
- Linux only: sessions are found through `/proc`.

## Security model

- A message is accepted only from a configured peer whose Tailscale node matches both its hostname and its
  exact tag or login. The sender is taken from Tailscale, never from the message.
- The local MCP endpoint needs a per-machine token. Sandboxed agents reach it through the sbx gateway, never directly.
- Messages from other agents are untrusted data. The workspace instructions tell your agent to report what a
  message asks rather than act on it.
- The API token stays on the network owner's machine (`~/.config/agent-relay/api-token`, mode 0600); it can edit
  the tailnet policy, so don't share it.
- Limits: 16 KB per message, 30 messages per minute per sender. Relays are offline when no session runs; there
  is no store-and-forward.

## Build

`just build` for this machine, `just dist` for release binaries (Linux, macOS, Windows), checksums and `install.sh`.
`just release v0.1.0` tags, builds and publishes a GitHub release.
