# Changelog

All notable changes to the CyberKiller agent. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [semantic versioning](https://semver.org/).

## [4.8.0] - 2026-06-10

### Added
- **One-command UX.** `sudo ./cyberkiller-agent` is now the only command a
  player needs. First run prompts for the invite token, connects, and opens a
  live terminal dashboard; every run after auto-resumes the saved session.
- **Live TUI dashboard** (pure stdlib — no new dependencies, still auditable):
  embedded ghost mascot rendered as truecolor half-blocks that scale to the
  terminal, btop-style braille bandwidth graphs driven by the real `wg0`
  interface (rx/tx bytes + packets, pps, totals, peak), responsive to resize.
  Auto color-depth detection (truecolor everywhere; degrades gracefully).
- Animated startup and shutdown sequences.

### Changed
- Default invocation goes straight to connect + dashboard. The `connect`,
  `disconnect`, `status`, and `submit` subcommands still work; `tui --demo`
  runs the dashboard with simulated traffic for screenshots/testing.
- Auto-update now re-execs straight into the dashboard when a session exists.

## [4.7.7] - 2026-06-09

### Added
- High-visibility "WHAT NOW" panel printed on successful connect (foreground
  and background modes). Spells out how to disconnect, where to find logs,
  hub URL, bug report URL, and rules link — so first-time players don't have
  to grep for it.
- Public-DNS fallback (`https://cyberkiller.net/api`) appended to the API URL
  variant list. An agent built with a broken or unreachable `defaultAPI` can
  still self-heal via auto-update without requiring the WireGuard tunnel,
  because the fallback is reachable pre-tunnel over the open internet.

### Changed
- Official binary now builds with `defaultAPI=https://cyberkiller.net/api`
  (was the arena-internal `http://10.66.0.1:8082`, which created a
  chicken-and-egg problem: agents couldn't verify their token before the
  tunnel was up because the tunnel was what made the API reachable).

### Fixed
- First-time connect no longer fails with "server unreachable" on a clean
  install when the agent has no prior state.
- Auto-update path now works even if every other variant is unreachable,
  thanks to the public-DNS fallback.

## [4.7.6] - 2026-06-08

### Added
- Open-source release. Source code now lives at
  <https://github.com/aaron-kidwell/cyberkiller-agent>.
- `SECURITY.md` with threat model + responsible disclosure contact.
- Build instructions for reproducing the official binary from source.

### Changed
- `defaultAPI` now empty in source; production releases bake it in at build
  time via `-ldflags`. Building without it yields an agent that refuses to
  start until `--api` or `CK_API` is supplied.
- Binary is stripped (`-trimpath -ldflags="-s -w"`) — no symbol tables, no
  embedded filesystem paths.

### Fixed
- Auto-update no longer panics when the server briefly returns a non-200.
- Cleaner error message when the saved state file is missing.

## [4.7.5] - 2026-06-04

### Added
- LAN fallback URL (`lanFallback`) set at build time for same-LAN testers
  whose home router doesn't hairpin NAT.
- Server-version detection: agent now self-updates on launch when the
  control plane reports a newer version.

### Changed
- Heartbeat interval reduced from 30s → 10s for faster online/offline
  detection in the hub.

## [4.7.0] - 2026-06-01

- Initial multiplayer GOAD support: foothold credential drops, KOTH bounty
  awards on round rotation, cross-domain trust-aware flag scanning.
