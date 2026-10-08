package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const maxCyclePackageList = 100
const maxAPTInventoryOutputBytes = maxStructuredCommandOutputBytes

// The script is deliberately fixed: package names and policy values never become
// Python source. It emits every version newer than the installed version; only
// the display list is capped.
const aptInventoryScript = `import apt, apt_pkg, json
cache = apt.Cache()
items = []
for package in cache:
    name = package.name
    if not package.is_installed:
        continue
    installed = package.installed.version if package.installed is not None else ""
    candidate = package.candidate
    if not installed:
        continue
    newer_available = any(apt_pkg.version_compare(version.version, installed) > 0 for version in package.versions)
    if not newer_available:
        continue
    candidate_version = candidate.version if candidate is not None else ""
    candidate_newer = apt_pkg.version_compare(candidate_version, installed) > 0 if candidate_version else None
    origins = []
    for origin in candidate.origins if candidate is not None else []:
        origins.append({
            "origin": getattr(origin, "origin", ""),
            "label": getattr(origin, "label", ""),
            "codename": getattr(origin, "codename", ""),
            "archive": getattr(origin, "archive", ""),
            "site": getattr(origin, "site", ""),
            "trusted": bool(getattr(origin, "trusted", False)),
        })
    try:
        held = package._pkg.selected_state == apt_pkg.SELSTATE_HOLD
    except Exception:
        held = None
    items.append({"name": name, "installed": installed, "candidate": candidate_version,
                  "newer_available": newer_available,
                  "candidate_newer": candidate_newer,
                  "priority": int(getattr(candidate, "policy_priority", 0)) if candidate is not None else 0,
                  "held": held, "origins": origins})
print(json.dumps({"packages": items}, separators=(",", ":")))`

type inventoryOrigin struct {
	Origin   string `json:"origin"`
	Label    string `json:"label"`
	Codename string `json:"codename"`
	Archive  string `json:"archive"`
	Site     string `json:"site"`
	Trusted  bool   `json:"trusted"`
}

type inventoryPackage struct {
	Name           string            `json:"name"`
	Installed      string            `json:"installed"`
	Candidate      string            `json:"candidate"`
	NewerAvailable bool              `json:"newer_available"`
	CandidateNewer *bool             `json:"candidate_newer"`
	Priority       int               `json:"priority"`
	Held           *bool             `json:"held"`
	Origins        []inventoryOrigin `json:"origins"`
}

type aptInventory struct {
	Packages []inventoryPackage `json:"packages"`
}

type inventoryResult struct {
	Counts entity.CycleCounts
	Valid  bool
}

func (h *Helper) readAPTInventory(ctx context.Context) (aptInventory, error) {
	output, err := h.commandWithOutputLimit(ctx, 2*time.Minute, maxAPTInventoryOutputBytes, "/usr/bin/python3", "-c", aptInventoryScript)
	if err != nil {
		return aptInventory{}, &operationError{code: "inventory_failed", stage: string(entity.CycleStageVerify), retryable: false, message: "APT inventory failed: " + sanitize(err.Error(), 512)}
	}
	var inventory aptInventory
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil || inventory.Packages == nil {
		return aptInventory{}, &operationError{code: "inventory_invalid", stage: string(entity.CycleStageVerify), message: "APT inventory output was invalid or incomplete"}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return aptInventory{}, &operationError{code: "inventory_invalid", stage: string(entity.CycleStageVerify), message: "APT inventory output had trailing data"}
	}
	return inventory, nil
}

func (h *Helper) classifyInventory(inventory aptInventory, policy entity.Policy, installable map[string]bool, blocked map[string]string, dryRunKnown bool, repositories []entity.Repository) inventoryResult {
	result := inventoryResult{Valid: dryRunKnown}
	packages := make([]entity.CyclePackage, 0, len(inventory.Packages))
	for _, pkg := range inventory.Packages {
		state, reason := "unknown", "candidate eligibility could not be determined"
		allowed, originsKnown := candidateAllowed(pkg, policy, h.currentCodename(), repositories)
		switch {
		case pkg.Name == "" || pkg.Installed == "" || !pkg.NewerAvailable:
			result.Counts.Unknown++
		case pkg.Held == nil:
			result.Counts.Unknown++
		case *pkg.Held:
			state, reason = "held", "package is held"
			result.Counts.Held++
		case pkg.Origins == nil || inventoryOriginIncomplete(pkg.Origins):
			result.Counts.Unknown++
		case !originsKnown:
			result.Counts.Unknown++
		case !allowed:
			state, reason = "excluded", "candidate is outside the effective authorized repositories"
			result.Counts.Excluded++
		case pkg.Candidate == "" || pkg.CandidateNewer == nil:
			result.Counts.Unknown++
		case !*pkg.CandidateNewer:
			state, reason = "blocked", "APT pin selected a candidate older than the installed package despite a newer version being available"
			result.Counts.Blocked++
		case pkg.Priority <= 0:
			state, reason = "blocked", "candidate has non-positive APT pin priority"
			result.Counts.Blocked++
		case !dryRunKnown:
			result.Counts.Unknown++
		case !installable[pkg.Name]:
			if detail, ok := blocked[pkg.Name]; ok {
				state, reason = "blocked", detail
				result.Counts.Blocked++
			} else {
				result.Counts.Unknown++
			}
		default:
			state, reason = "eligible", ""
			result.Counts.Eligible++
		}
		result.Counts.Candidates++
		if len(packages) < maxCyclePackageList {
			packages = append(packages, entity.CyclePackage{Name: sanitize(pkg.Name, 160), State: state, Reason: sanitize(reason, 256)})
		}
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	result.Counts.Packages = packages
	result.Counts.Pending = result.Counts.Eligible + result.Counts.Held + result.Counts.Excluded + result.Counts.Blocked + result.Counts.Unknown
	result.Valid = result.Valid && result.Counts.Unknown == 0
	return result
}

func candidateAllowed(pkg inventoryPackage, policy entity.Policy, release string, repositories []entity.Repository) (bool, bool) {
	if len(pkg.Origins) == 0 || inventoryOriginIncomplete(pkg.Origins) {
		return false, false
	}
	if (policy.Mode == entity.ModeSecurity || policy.Mode == entity.ModeOfficialAll) && release == "" {
		return false, false
	}
	originsKnown := true
	for _, origin := range pkg.Origins {
		if !origin.Trusted {
			continue
		}
		repo := entity.Repository{Origin: origin.Origin, Label: origin.Label, Codename: origin.Codename, Archive: origin.Archive, Site: origin.Site, Trusted: origin.Trusted}
		switch policy.Mode {
		case entity.ModeSecurity:
			if release != "" && isOfficial(repo) && isSecurityOrigin(origin, release) {
				return true, true
			}
		case entity.ModeOfficialAll:
			if isOfficial(repo) && isCurrentReleaseOrigin(origin, release) {
				return true, true
			}
		case entity.ModeCustom:
			matched := matchingCustomRepositories(origin, repositories)
			if len(matched) != 1 {
				originsKnown = false
				continue
			}
			detected := matched[0]
			if (detected.Official || policy.ConfirmThirdParty) && containsString(policy.AllowedRepositories, detected.ID) {
				return true, true
			}
		}
	}
	return false, originsKnown
}

func inventoryOriginIncomplete(origins []inventoryOrigin) bool {
	for _, origin := range origins {
		if origin.Origin == "" || origin.Label == "" || origin.Codename == "" || origin.Site == "" {
			return true
		}
	}
	return false
}

func matchingCustomRepositories(origin inventoryOrigin, repositories []entity.Repository) []entity.Repository {
	var matches []entity.Repository
	seen := make(map[string]bool)
	for _, repo := range repositories {
		if !repo.Trusted || repo.Origin != origin.Origin || repo.Label != origin.Label || repo.Codename != origin.Codename || repo.Archive != origin.Archive || repositorySiteHost(repo.Site) != origin.Site {
			continue
		}
		if !seen[repo.ID] {
			seen[repo.ID] = true
			matches = append(matches, repo)
		}
	}
	return matches
}

func repositorySiteHost(value string) string {
	return value
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func isSecurityOrigin(origin inventoryOrigin, release string) bool {
	if release == "" {
		return false
	}
	codename := strings.ToLower(origin.Codename)
	release = strings.ToLower(release)
	if codename == release+"-security" {
		return true
	}
	archive := strings.ToLower(origin.Archive)
	return codename == release && strings.HasPrefix(archive, release+"-") && strings.Contains(archive, "security")
}

func isCurrentReleaseOrigin(origin inventoryOrigin, release string) bool {
	if release == "" || origin.Codename == "" {
		return false
	}
	base := strings.ToLower(origin.Codename)
	version := strings.ToLower(release)
	if base != version && !strings.HasPrefix(base, version+"-") {
		return false
	}
	if base == version {
		// Exact codename pins aliases such as stable/oldstable to this release.
		return true
	}
	suffix := strings.TrimPrefix(base, version+"-")
	return allowedPocket(suffix)
}

func allowedPocket(value string) bool {
	value = strings.ToLower(value)
	for _, pocket := range []string{"security", "updates", "backports", "proposed", "esm", "esm-infra", "esm-apps"} {
		if value == pocket || strings.HasPrefix(value, pocket+"-") {
			return true
		}
	}
	return false
}

var packageToken = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9.+:~-]*`)

func parseUnattendedDryRun(output []byte) (map[string]bool, map[string]string, bool) {
	text := string(output)
	const marker = "Packages that will be upgraded:"
	index := strings.Index(text, marker)
	blocked := make(map[string]string)
	for _, item := range []struct{ marker, reason string }{{"Packages that are kept back:", "APT kept the package back"}, {"Packages that are blacklisted:", "package is blacklisted by unattended-upgrade"}} {
		for rest := text; ; {
			at := strings.Index(rest, item.marker)
			if at < 0 {
				break
			}
			line := strings.SplitN(rest[at+len(item.marker):], "\n", 2)[0]
			for _, name := range packageToken.FindAllString(line, -1) {
				blocked[strings.Trim(name, ",.")] = item.reason
			}
			rest = rest[at+len(item.marker):]
		}
	}
	if index < 0 {
		lower := strings.ToLower(text)
		for _, known := range []string{"no packages found that can be upgraded unattended", "no packages can be upgraded unattended", "no packages to upgrade"} {
			if strings.Contains(lower, known) {
				return map[string]bool{}, blocked, true
			}
		}
		return nil, blocked, false
	}
	rest := text[index+len(marker):]
	packages := make(map[string]bool)
	lines := strings.Split(rest, "\n")
	started := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if started {
				break
			}
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "packages that ") || strings.HasPrefix(lower, "no packages ") {
			break
		}
		started = true
		for _, item := range packageToken.FindAllString(line, -1) {
			packages[strings.Trim(item, ",.")] = true
		}
	}
	return packages, blocked, true
}

func (h *Helper) runDpkgAudit(ctx context.Context, store *cycleStore) error {
	output, _, err := h.cycleCommand(ctx, store, time.Minute, string(entity.CycleStageVerify), "dpkg", "--audit")
	if err != nil {
		return &operationError{code: "dpkg_audit_failed", stage: string(entity.CycleStageVerify), retryable: true, message: sanitize(err.Error(), 512)}
	}
	if strings.TrimSpace(string(output)) != "" {
		return &operationError{code: "dpkg_inconsistent", stage: string(entity.CycleStageVerify), message: "dpkg reports incomplete package configuration: " + sanitize(string(output), 512)}
	}
	return nil
}

func (h *Helper) currentCodename() string {
	data, err := os.ReadFile(h.path("/etc/os-release"))
	if err != nil {
		return ""
	}
	values := map[string]string{}
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = strings.Trim(value, "\"'")
		}
	}
	if values["UBUNTU_CODENAME"] != "" {
		return values["UBUNTU_CODENAME"]
	}
	return values["VERSION_CODENAME"]
}

func isNetworkRefreshFailure(output []byte, err error) bool {
	text := strings.ToLower(string(output) + " " + fmt.Sprint(err))
	for _, definitive := range []string{"signature", "not signed", "no_pubkey", "conflicting values set for option", "malformed entry", "invalid configuration", "does not have a release file"} {
		if strings.Contains(text, definitive) {
			return false
		}
	}
	return true
}

func validateRefreshOutput(output []byte) error {
	text := strings.ToLower(string(output))
	for _, marker := range []string{"failed to fetch", "some index files failed to download", "could not resolve", "temporary failure resolving", "connection failed", "connection timed out", "could not connect", "gpg error", "no_pubkey", "not signed", "signatures couldn't be verified", "hash sum mismatch", "does not have a release file", "conflicting values set for option", "malformed entry"} {
		if strings.Contains(text, marker) {
			return errors.New("APT index refresh was partial: " + sanitize(string(output), 512))
		}
	}
	return nil
}

func cycleInventoryError(err error) *operationError {
	var typed *operationError
	if errors.As(err, &typed) {
		return typed
	}
	return &operationError{code: "inventory_failed", stage: string(entity.CycleStageVerify), message: fmt.Sprintf("inventory failed: %v", err)}
}
