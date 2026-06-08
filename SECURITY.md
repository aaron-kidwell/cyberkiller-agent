# Security

## Threat model

The CyberKiller agent runs **as root** on the player's attack VM. It needs that privilege to manage the WireGuard interface and `iptables` rules. This document describes what the agent does, what it deliberately does *not* do, and the trust assumptions that apply.

### What the agent assumes you trust

- **The CyberKiller server** at the URL baked into `defaultAPI` (or passed via `--api`/`CK_API`). The agent talks to this server for everything: registration, heartbeats, flag submissions, **and updates**. If the server is compromised, an attacker can push a malicious binary update that runs as root on your machine. This is the same trust model as HackTheBox's `connection-pack`, Hak5's Cloud C2 agent, and most CTF range tooling — but it is a real trust commitment, and you should know about it.
- **The TLS chain to that server**. Built using Go's `crypto/tls`, validating against the system root CAs. The agent does **not** disable cert verification.
- **The host you run it on**. The agent doesn't try to escape, hide, or persist past a reboot — but it does run as root, so a malicious or buggy build has the same blast radius any root process does.

### What the agent does NOT do

The following are deliberately absent. If you observe any of them, **open a GitHub issue and treat the binary as suspect**:

- ❌ Read your `~/.ssh/`, `~/.bash_history`, browser data, password managers, or anything outside its own state files
- ❌ Phone home anywhere except the configured `defaultAPI`
- ❌ Install systemd units, cron jobs, login items, or other persistence mechanisms
- ❌ Disable antivirus, EDR, host firewalls, or system security features
- ❌ Spawn arbitrary processes (only `wg`, `wg-quick`, `iptables`, `ping`)
- ❌ Listen on any port (it's outbound HTTPS + UDP-via-WireGuard only)
- ❌ Modify your system PATH, shell profile, or hosts file

The agent **does** modify:

- `/etc/wireguard/wg0.conf` (overwrites it with the arena tunnel config)
- A single `iptables` egress DROP rule on the `wg0` interface for `! -d 10.66.0.0/16` (kill-switch — prevents non-arena traffic from leaking through the VPN). Removed on disconnect.

## Self-update behavior

Auto-update is on by default. On every launch the agent:

1. `GET /agent/version` to check the latest version string.
2. If different from compiled-in `agentVersion`, `GET /agent/linux-amd64` to fetch the new binary.
3. Write to `cyberkiller-agent.update`, `os.Rename` over the current executable, `syscall.Exec` to re-execute.

**There is no signature verification.** Server-pushed code runs immediately as root. Mitigations available to paranoid users:

- Build from source pinned to a specific commit (`git checkout <sha> && go build ...`).
- Block the agent's outbound connection to the API host except during play windows.
- Run the agent in a disposable VM that gets rolled back after each session.

A signed-binary release flow (cosign or minisign) is on the roadmap; PRs welcome.

## Reporting vulnerabilities

If you find a security issue in the agent (privilege escalation, arbitrary file write outside the documented paths, unbounded memory exhaustion, etc.), please **do not** open a public issue.

Report it through the platform's bug-report form while logged in:

- <https://cyberkiller.net/report>

Set category to **BUG** and include enough detail to reproduce. The operator will respond and coordinate a fix before any public disclosure.

## Reproducible builds

The official binary is built with `-trimpath -ldflags="-s -w"` so it carries no local filesystem paths or symbols. Verify by building yourself with the same flags and comparing SHA256:

```bash
go build -trimpath \
  -ldflags "-s -w -X main.defaultAPI=https://cyberkiller.net/api" \
  -o cyberkiller-agent .

sha256sum cyberkiller-agent
# Compare with the SHA256 published on the release page.
```

If the hashes differ, either the release was rebuilt without `-trimpath`, your Go toolchain version differs, or the published binary was tampered with. Open an issue if you suspect the latter.

## What's NOT in scope

- The CyberKiller server itself is not open source. Vulnerabilities in the server should be reported to the same email above. Authorized server-side security testing within your own account is welcomed; testing against other players' data or attempting to disrupt the live arena is not.
- The arena machines (GOAD, vulnerable Docker targets) are intentionally vulnerable for player practice. Findings there are gameplay, not vulnerability reports.
