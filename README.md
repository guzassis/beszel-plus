# Beszel Plus

Beszel Plus is a public fork of [Beszel](https://github.com/henrygd/beszel), a lightweight server monitoring platform with Docker statistics, historical data, alerts, multi-user access, OAuth/OIDC, automatic backups, and API access.

The fork preserves compatibility with the original project while adding operational features used by Beszel Plus deployments.

[![Fork original version](https://img.shields.io/badge/fork_original-v0.18.2-2563eb)](https://github.com/henrygd/beszel/releases/tag/v0.18.2)
[![Beszel Plus version](https://img.shields.io/badge/Beszel_Plus-v0.2.5-7c3aed)](https://github.com/guzassis/beszel-plus/releases/tag/v0.2.5)
[![MIT license](https://img.shields.io/github/license/henrygd/beszel?color=%239944ee)](https://github.com/henrygd/beszel/blob/main/LICENSE)
[![Crowdin](https://badges.crowdin.net/beszel/localized.svg)](https://crowdin.com/project/beszel)

![Screenshot of Beszel dashboard and system page, side by side. The dashboard shows metrics from multiple connected systems, while the system page shows detailed metrics for a single system.](https://henrygd-assets.b-cdn.net/beszel/screenshot-new.png)

## Features

- **Lightweight**: Smaller and less resource-intensive than leading solutions.
- **Simple**: Easy setup with little manual configuration required.
- **Docker stats**: Tracks CPU, memory, and network usage history for each container.
- **Alerts**: Configurable alerts for CPU, memory, disk, bandwidth, temperature, load average, and status.
- **Multi-user**: Users manage their own systems. Admins can share systems across users.
- **OAuth / OIDC**: Supports many OAuth2 providers. Password auth can be disabled.
- **Automatic backups**: Save to and restore from disk or S3-compatible storage.
- **OS update monitoring**: Reports `unattended-upgrades`, APT timers, pending/security/manual updates, reboot state, stale data, and partial collection errors.
- **OS update management**: Administrators can install dependencies, inspect repositories, validate/apply update policies, run dry-runs, and start updates from the Hub.
- **Least-privilege maintenance**: The Agent remains unprivileged and delegates enumerated APT/systemd operations to a root one-shot helper over a protected local Unix socket.
- **Self-repairing maintenance install**: Re-running the Agent installer repairs the v0.0.3 socket/template association, replaces the helper with the matching release, verifies socket activation, and reapplies the initial policy only when its state is missing.
- **Transactional maintenance upgrades**: The v0.1.3 installer bounds legacy helper detection, validates both new binaries, installs them atomically, starts the Agent only after policy setup, and restores binaries and systemd units if installation fails or is interrupted.
- **Resilient APT policy setup**: Real APT lock ownership is detected from known lock files and `/proc/locks`; lock-like output without a kernel holder is retried only once and reported as a dry-run error instead of causing a false 75-second wait, while genuinely blocked policies remain pending for Agent retry.
- **Kernel-accurate APT coordination**: Lock device and inode identifiers are parsed numerically using the real `/proc/locks` format, including zero-padded devices used by `unattended-upgrades`; inspection failures postpone privileged work instead of being treated as an unlocked system.
- **Serialized update operations**: Collection and privileged maintenance share a local gate, preventing overlapping APT commands and duplicate collection cycles.
- **Partial eligibility reporting**: Eligibility dry-runs use the privileged helper. Temporary lock or helper failures preserve the rest of the update snapshot and the last successful eligibility value instead of showing a global collection error.
- **Verified connection refresh**: Re-running the systemd installer neutralizes stale `BESZEL_AGENT_*` overrides, verifies the running process configuration without printing secrets, and requires a post-restart WebSocket handshake before reporting complete success.
- **Independent version compatibility**: WebSocket and SSH negotiation advertise the preserved upstream protocol baseline (`v0.18.2`) while Hub, Agent, helper, installers, UI, and system data report the independent Beszel Plus release (`v0.2.5`).
- **Enrollment-safe install commands**: New systems and fingerprints are persisted in the Hub before the Linux command is copied; failed fingerprint creation removes the incomplete system and concurrent submissions are ignored.
- **Power management**: Administrators can configure a local power network, inspect Agent network/WOL readiness, send Wake-on-LAN from the Hub, schedule a safe shutdown, and cancel a pending shutdown from the system page.
- **Hub-local WOL**: Magic packets are generated directly by the Hub and sent three times over UDP 7 or 9. No root process, external command, listening port, or Tailscale-specific dependency is required.
- **Guarded shutdown**: Shutdown requests travel over the existing authenticated Hub ↔ Agent channel and the restricted helper refuses them while APT/dpkg or another maintenance operation is active. systemd owns the timer; no sleeping process is left behind.
- **Transactional Agent upgrades**: The v0.2.1 installer drains new maintenance requests, postpones safely when a critical operation is active, preserves omitted management flags, replaces executables atomically, and validates rollback before reporting success.
- **Context-aware APT coordination**: The v0.2.2 installer identifies actual kernel lock owners, waits up to 60 seconds only when a missing dependency requires APT, and otherwise continues safely alongside `unattended-upgrades`. It never kills or suspends package-management processes.
- **Hardened native installers**: Agent connection credentials are stored in a root-only environment file, omitted upgrade settings are preserved, and both Agent and Hub installers serialize concurrent runs, validate checksums and component versions, write atomically, and roll back failed transactions.
- **Stable installer workspace**: Release assets are handled through absolute temporary paths, and privileged commands always run from `/`, preventing deleted download directories from leaking into policy validation.
- **Reproducible native releases**: The v0.2.5 pipeline builds complete Go command packages with a pinned GoReleaser version, validates the full six-archive Linux matrix, and runs the same release as a non-publishing snapshot on every push to `main`.
- **Upgrade diagnostics**: Run the Agent installer with `--diagnose` to inspect installed component versions, service state, active maintenance, APT locks, pending policy, power management, and upgrade readiness without changing the installation.
<!-- - **REST API**: Use or update your data in your own scripts and applications. -->

## Beszel Plus update management

The management flow reuses the authenticated Hub ↔ Agent transport already provided by Beszel:

```text
Beszel Plus Hub
  → existing WebSocket or SSH transport
  → unprivileged beszel-agent
  → /run/beszel-maintenance.sock (root:beszel, 0660)
  → root one-shot maintenance helper
  → validated APT and systemd operations
```

No new TCP port, client HTTP API, Tailscale identity, `tsnet`, or Tailscale SSH integration is introduced. Beszel Plus does not depend on Tailscale, but it can naturally use a tailnet because it reuses the existing Hub ↔ Agent channel.

Supported managed platforms are Debian, Ubuntu, and Raspberry Pi OS. Available policies are:

- `monitor_only`: monitoring without automatic package installation.
- `security`: official security updates only; this is the default for new managed installations.
- `official_all`: all official distribution/vendor repositories, including Raspberry Pi Foundation where applicable.
- `custom`: explicitly selected repositories, with mandatory confirmation for third-party sources.

Automatic reboot is always disabled by default. The helper accepts only versioned, typed operations; it does not accept shell commands, arbitrary paths, environment variables, or free-form command arguments. Existing APT configuration is preserved during Agent upgrades and is never silently replaced with a broader policy.

See [Automatic updates](supplemental/guides/automatic-updates.md) for installer flags, security details, troubleshooting, logs, policy behavior, and helper removal.

## Architecture

Beszel Plus retains the two original components and adds an optional local helper:

- **Hub**: A web application built on [PocketBase](https://pocketbase.io/) that provides a dashboard for viewing and managing connected systems.
- **Agent**: Runs as the unprivileged `beszel` user on each monitored system and communicates metrics and typed responses to the Hub.
- **Maintenance helper**: A root one-shot process activated by systemd through a restricted Unix socket only when a validated privileged operation is required.

The v0.2.0 release is deliberately a native Linux/systemd release. Official artifacts are built only for Debian, Ubuntu, and Raspbian on `linux/amd64` and `linux/arm64`: one Hub archive, one Agent archive, one matching maintenance-helper archive, and a checksum manifest per release. Docker image publishing, Docker installation, Windows, macOS, FreeBSD, 32-bit ARM, MIPS, RISC-V, and other package formats are outside the official v0.2.0 matrix.

## Installation

The Linux installation command generated by the Hub enables update management with a conservative security-only policy. The relevant installer flags are:

```text
--agent-auto-update=true|false
--os-update-management=true|false
--os-update-policy=monitor|security|official-all
--power-management=true|false
--wait-for-maintenance=0..300
--wait-for-apt=0..3600
--diagnose
```

The Hub installer enables power management by default and accepts `--power-interface <name>` to prefer one local interface. Agent power management is opt-in. When enabled, the installer installs `ethtool` once if it is missing, then the Agent collects diagnostics without installing packages or executing arbitrary shell commands during normal monitoring.

The deprecated `--auto-update` flag remains an alias for `--agent-auto-update`. Updating the Beszel Agent binary and updating operating-system packages are separate controls.

## Versioning

Beszel Plus tracks the original fork baseline as `v0.18.2` and uses an independent release sequence for its own changes. The current Beszel Plus release is `v0.2.5`. Hub, Agent, maintenance helper, frontend, installers, update checks, and release assets use the same product version injected from `internal/buildinfo`; upstream protocol and Go module compatibility are tracked separately and are not displayed as the Beszel Plus product version.

Each Beszel Plus release must update this README whenever user-visible functionality, installation behavior, supported platforms, security architecture, or operational requirements change.

## Getting started

The original [Beszel quick start guide](https://beszel.dev/guide/getting-started) remains applicable. Beszel Plus-specific behavior is documented in this repository under [`supplemental/guides`](supplemental/guides).

## Screenshots

![Dashboard](https://beszel.dev/image/dashboard.png)
![System page](https://beszel.dev/image/system-full.png)
![Notification Settings](https://beszel.dev/image/settings-notifications.png)

## Supported metrics

- **CPU usage** - Host system and Docker / Podman containers.
- **Memory usage** - Host system and containers. Includes swap and ZFS ARC.
- **Disk usage** - Host system. Supports multiple partitions and devices.
- **Disk I/O** - Host system. Supports multiple partitions and devices.
- **Network usage** - Host system and containers.
- **Load average** - Host system.
- **Temperature** - Host system sensors.
- **GPU usage / power draw** - Nvidia, AMD, and Intel.
- **Battery** - Host system battery charge.
- **Containers** - Status and metrics of all running Docker / Podman containers.
- **S.M.A.R.T.** - Host system disk health (includes eMMC wear/EOL and Linux mdraid array health via sysfs when available).

## Help and discussion

Please search existing issues and discussions before opening a new one. I try my best to respond, but may not always have time to do so.

#### Bug reports and feature requests

Beszel Plus bug reports and feature requests can be posted on [GitHub issues](https://github.com/guzassis/beszel-plus/issues). Upstream Beszel issues should be reported to the original project.

#### Support and general discussion

Support requests and general discussion can be posted on [GitHub discussions](https://github.com/henrygd/beszel/discussions) or the community-run [Matrix room](https://matrix.to/#/#beszel:matrix.org): `#beszel:matrix.org`.

## License

Beszel is licensed under the MIT License. See the [LICENSE](LICENSE) file for more details.
