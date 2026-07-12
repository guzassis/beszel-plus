# Security Policy

## Reporting a Vulnerability

If you find a vulnerability in the latest version, please [submit a private advisory](https://github.com/guzassis/beszel-plus/security/advisories/new).

If it's low severity (use best judgement) you may open an issue instead of an advisory.

## Vulnerability scan notes

As of the Go 1.26.5 dependency scan, `govulncheck -show verbose ./...` reports no reachable vulnerabilities. It may still report `GO-2026-5932` at module level for `golang.org/x/crypto/openpgp`. The project requires `golang.org/x/crypto` through `golang.org/x/crypto/ssh`; `openpgp` is not present in the compiled package dependency graph. Upstream does not provide a fixed `x/crypto` version for this advisory, so the module-only finding is documented here rather than prompting an unrelated replacement of the SSH dependency.
