# cyberkiller-agent

The official Linux player agent for the [CyberKiller](https://cyberkiller.net) competitive hacking arena.

It does three things:

1. Brings up a **WireGuard tunnel** (`wg0`) to the arena network so you can reach the practice machines at `10.66.20.X`.
2. **Heartbeats** the control plane so the hub knows you're online and your handle appears in the connected list / leaderboard.
3. **Submits flags / kills / KOTH claims** you capture during play.

That's the whole agent. It's a single Go file (~900 lines) and you're encouraged to read it before running it as root.

---

## Why open source?

The agent runs as root (it needs to manage WireGuard + `iptables`). Asking strangers to run unaudited root binaries is unreasonable. This repo lets you:

- **Read the source** before installing.
- **Build from source** if you don't trust the hosted binary.
- **Verify the published release** by reproducing the build and comparing SHA256.

See [`SECURITY.md`](./SECURITY.md) for the threat model.

---

## Install

### Quick (download the official binary)

```bash
curl -O https://cyberkiller.net/api/agent/linux-amd64
chmod +x linux-amd64
sudo ./linux-amd64 <YOUR_INVITE_TOKEN>
```

You get your invite token by signing up at <https://cyberkiller.net/signup>.

### Build from source

Requirements: Go 1.22+, `wireguard-tools`, `iptables`, root.

```bash
git clone https://github.com/aaron-kidwell/cyberkiller-agent.git
cd cyberkiller-agent
go build -trimpath \
  -ldflags "-s -w -X main.defaultAPI=https://cyberkiller.net/api" \
  -o cyberkiller-agent .
sudo ./cyberkiller-agent <YOUR_INVITE_TOKEN>
```

Self-hosted CyberKiller install? Point at your own range:

```bash
go build -trimpath \
  -ldflags "-s -w -X main.defaultAPI=https://your-range.example/api" \
  -o cyberkiller-agent .
```

If you build with `defaultAPI` empty, the agent refuses to start unless you pass `--api` or set `CK_API`.

### Verify the official binary

Every release publishes the binary plus its SHA256. Build from source yourself and compare:

```bash
sha256sum cyberkiller-agent
# compare with the value shown on the GitHub Releases page
```

---

## Usage

| Command | What it does |
|---|---|
| `sudo ./cyberkiller-agent INVITE_TOKEN` | First-time setup. Registers your WireGuard public key with the arena, brings up `wg0`, starts heartbeating. |
| `sudo ./cyberkiller-agent connect` | Re-connect using the saved state (after `disconnect` or a reboot). |
| `sudo ./cyberkiller-agent disconnect` | Tear down the WireGuard tunnel. |
| `sudo ./cyberkiller-agent status` | Print tunnel state, latest handshake, version. |
| `sudo ./cyberkiller-agent submit --flag <value>` | Submit a captured flag. |
| `sudo ./cyberkiller-agent submit --kill <ip>` | Submit a kill against a target VM. |

State is persisted in `/var/run/cyberkiller-agent.json`. Logs go to `/var/log/cyberkiller-agent.log`.

---

## What the agent touches on your machine

Full list (grep the source if you want to verify):

- **Reads/writes** `/var/run/cyberkiller-agent.json` (state) and `/var/log/cyberkiller-agent.log` (logs).
- **Writes** `/etc/wireguard/wg0.conf` with the WireGuard config the server returns.
- **Runs** `wg-quick up wg0`, `wg-quick down wg0`, `wg show wg0`, `wg genkey`, `wg pubkey`.
- **Adds an iptables egress rule** on the `wg0` interface so the tunnel can only reach `10.66.0.0/16` (kill-switch — your traffic to the wider internet never leaves through the arena VPN). Removed on disconnect.
- **HTTPS** to the configured `defaultAPI` host for `/register`, `/heartbeat`, `/flag/submit`, `/kill/target`, `/koth/claim`, `/agent/version`, `/agent/linux-amd64` (update).
- **No** persistence in systemd / cron / autostart.
- **No** telemetry beyond the documented API endpoints above.
- **No** access to your home directory, browser data, SSH keys, or shell history.

If you see the agent doing anything else, that's a bug — open an issue.

---

## Auto-update

When launched, the agent checks `GET /agent/version` and, if a newer version is available, downloads `/agent/linux-amd64` and replaces itself. This is documented in `applyUpdate()`. There is currently **no offline signature verification** — if the server is compromised, the agent can be replaced. This is the same model HTB and similar platforms use. If your threat model rejects this, build from source pinned to a commit and run with `--api` to a server you trust.

---

## License

[MIT](./LICENSE) — do what you want with it.

## Security

See [SECURITY.md](./SECURITY.md). Report vulnerabilities privately via the contact info there.
