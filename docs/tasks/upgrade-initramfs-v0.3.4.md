# Upgrade installation diagnosis and v0.3.4

## Objective and evidence

Resolve the failed install phase of cycle `cycle-00000000000000000003` without guessing from kept-back warnings. Its unattended-upgrade log records a successful dry-run, then `installArchives() failed` at 2026-10-07 09:55:41. The matching APT terminal log accessible in the workspace identifies the fatal `dracut` configuration trigger: `/lib/modules/7.0.0-38-generic` was invisible during initramfs generation. That module tree exists outside the service. The shipped helper uses `ProtectKernelModules=yes`, which systemd documents as making `/usr/lib/modules` inaccessible. A later transaction at 2026-10-08 06:02 successfully configured dracut and generated the same image; the user's current dpkg audit is empty.

This is an **upgrade installation failure**, after index refresh. The sg3-utils dependency group installed but triggered initramfs reconstruction; kept-back messages are aftermath, not evidence of local pins. The installer service template is a product defect, not a missing kernel package established by these logs.

## Changes and acceptance

- Expose module files while denying module-loading capabilities/syscalls; allow Netlink sockets for package hooks. Preserve the root-helper/unprivileged-Agent boundary.
- Correct missing-Suite vendor classification without authorizing third-party updates implicitly.
- Ignore dry-run APT history entries as completed upgrades.
- Persist current-install dpkg/initramfs error details and describe incomplete inventory accurately.
- Test actual isolated dpkg 1.0 → 2.0 installation with real dracut/initramfs, plus legacy-versus-fixed native systemd behavior and `_apt` credential drops. No host package database or boot image is modified by fixtures.
- Align components/installers to v0.3.4; commit, PR, merge, tag and verify published assets.

## Checks and state

Local checks passed: `BESZEL_TEST_APT=1 make test`, targeted race tests for maintenance diagnostics/authorization and Agent APT history, real isolated dpkg/dracut installation on Ubuntu 26.04, real distribution python-apt fixtures, frontend build, Biome for changed metadata, installer `sh -n`, `gofmt` and `git diff --check`.

Native systemd comparison is required in CI on Ubuntu 24.04; this workspace has no systemd PID 1 and Docker access is denied. Release packaging, PR validation and publication are in progress. The affected host must receive the new unit through the paired installer and run a new cycle; no production cycle was executed from this workspace.
