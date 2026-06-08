# Changelog

All notable changes to the CyberKiller agent. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [semantic versioning](https://semver.org/).

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
