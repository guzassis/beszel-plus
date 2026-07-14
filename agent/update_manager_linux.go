//go:build linux

package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

const maxUpdateLogBytes int64 = 256 * 1024

type updateCommandExecutor interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type osUpdateExecutor struct{}

func (osUpdateExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func collectUpdateStatus(ctx context.Context, opts updateOptions, systemd *systemdManager) (*updateentity.Status, error) {
	return collectLinuxUpdateStatus(ctx, opts, systemd, osUpdateExecutor{})
}

func collectLinuxUpdateStatus(ctx context.Context, opts updateOptions, systemd *systemdManager, executor updateCommandExecutor) (*updateentity.Status, error) {
	s := &updateentity.Status{
		PackageManager: "apt", InstallationState: updateentity.InstallationUnknown,
		ConfigurationState: updateentity.ConfigurationUnknown, TimerState: updateentity.TimerUnknown,
		ServiceState: updateentity.ServiceUnknown, LastResult: updateentity.ResultUnknown,
	}
	id, err := readOSRelease("/etc/os-release")
	if err != nil || (id != "debian" && id != "ubuntu" && id != "raspbian") {
		s.Supported = false
		s.OverallState = updateentity.OverallUnsupported
		return s, nil
	}
	s.Supported = true
	s.DataSources = append(s.DataSources, "os-release")

	collectInstallation(ctx, s, executor, opts.timeout)
	if s.InstallationState == updateentity.InstallationNotInstalled {
		collectReboot(s, opts.maxPackages)
		s.OverallState = updateentity.DeriveOverallState(s)
		return s, nil
	}
	collectConfiguration(ctx, s, executor, opts.timeout)
	collectSystemd(s, systemd)
	collectPending(ctx, s, executor, opts)
	collectReboot(s, opts.maxPackages)
	collectAptHistory(s, opts.maxPackages)
	if len(s.CollectionErrors) > 0 {
		s.LastError = s.CollectionErrors[0]
	}
	s.OverallState = updateentity.DeriveOverallState(s)
	return s, nil
}

func readOSRelease(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == "ID" {
			return strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'")), nil
		}
	}
	return "", errors.New("ID missing in os-release")
}

func collectInstallation(ctx context.Context, s *updateentity.Status, executor updateCommandExecutor, timeout time.Duration) {
	output, err := runUpdateStep(ctx, executor, timeout, "dpkg-query", "-W", "-f=${db:Status-Status}", "unattended-upgrades")
	if err == nil {
		s.DataSources = append(s.DataSources, "dpkg-query")
		if strings.TrimSpace(string(output)) == "installed" {
			s.InstallationState = updateentity.InstallationInstalled
		} else {
			s.InstallationState = updateentity.InstallationNotInstalled
		}
		return
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		s.DataSources = append(s.DataSources, "dpkg-query")
		s.InstallationState = updateentity.InstallationNotInstalled
		return
	}
	s.CollectionErrors = append(s.CollectionErrors, "package detection: "+sanitizeUpdateText(err.Error(), 256))
	if _, statErr := os.Stat("/usr/bin/unattended-upgrade"); statErr == nil {
		s.InstallationState = updateentity.InstallationInstalled
		s.DataSources = append(s.DataSources, "unattended-upgrade-binary-limited")
	}
}

func collectConfiguration(ctx context.Context, s *updateentity.Status, executor updateCommandExecutor, timeout time.Duration) {
	output, err := runUpdateStep(ctx, executor, timeout, "apt-config", "dump")
	if err != nil {
		s.ConfigurationState = updateentity.ConfigurationUnknown
		s.CollectionErrors = append(s.CollectionErrors, "configuration: "+sanitizeUpdateText(err.Error(), 256))
		return
	}
	s.DataSources = append(s.DataSources, "apt-config")
	values := parseAptConfig(output)
	updateLists, hasLists := values["APT::Periodic::Update-Package-Lists"]
	unattended, hasUnattended := values["APT::Periodic::Unattended-Upgrade"]
	if hasUnattended && !aptConfigEnabled(unattended) {
		s.ConfigurationState = updateentity.ConfigurationDisabled
		return
	}
	if hasUnattended && aptConfigEnabled(unattended) && hasLists && aptConfigEnabled(updateLists) {
		s.ConfigurationState = updateentity.ConfigurationEnabled
		return
	}
	if hasUnattended || hasLists {
		s.ConfigurationState = updateentity.ConfigurationPartial
		return
	}
	s.ConfigurationState = updateentity.ConfigurationMissing
}

func parseAptConfig(data []byte) map[string]string {
	result := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") || !strings.HasSuffix(line, ";") {
			continue
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, ";"))
		idx := strings.IndexAny(line, " \t")
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		value := strings.Trim(strings.TrimSpace(line[idx:]), "\"")
		result[key] = value // apt-config dump is effective config; last value wins.
	}
	return result
}

func aptConfigEnabled(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n > 0
	}
	return value == "true" || value == "yes" || value == "on"
}

func collectSystemd(s *updateentity.Status, manager *systemdManager) {
	if manager == nil {
		return
	}
	timerNames := []string{"apt-daily.timer", "apt-daily-upgrade.timer"}
	states := make([]updateentity.TimerState, 0, len(timerNames))
	for _, name := range timerNames {
		props, err := manager.getUpdateUnitDetails(name)
		unit := updateentity.UnitStatus{Name: name, State: updateentity.TimerUnknown}
		if err != nil {
			unit.State = updateentity.TimerNotFound
		} else {
			unit.State = timerState(props)
			unit.Last = systemdTimestamp(props["LastTriggerUSec"])
			unit.Next = systemdTimestamp(props["NextElapseUSecRealtime"])
			s.DataSources = appendUnique(s.DataSources, "systemd")
		}
		states = append(states, unit.State)
		s.Timers = append(s.Timers, unit)
		if name == "apt-daily.timer" && unit.Last != nil {
			s.LastCheckAt = unit.Last
		}
	}
	s.TimerState = aggregateTimerState(states)
	props, err := manager.getUpdateUnitDetails("apt-daily-upgrade.service")
	if err != nil {
		props, err = manager.getUpdateUnitDetails("unattended-upgrades.service")
	}
	if err != nil {
		s.ServiceState = updateentity.ServiceNotFound
		return
	}
	active := stringProp(props, "ActiveState")
	result := stringProp(props, "Result")
	switch {
	case active == "active" || active == "activating":
		s.ServiceState, s.LastResult = updateentity.ServiceRunning, updateentity.ResultRunning
	case active == "failed" || (result != "" && result != "success"):
		s.ServiceState, s.LastResult = updateentity.ServiceFailed, updateentity.ResultFailed
	case result == "success":
		s.ServiceState, s.LastResult = updateentity.ServiceSuccess, updateentity.ResultSuccess
	default:
		s.ServiceState = updateentity.ServiceInactive
		s.LastResult = updateentity.ResultNeverRun
	}
	if ts := systemdTimestamp(props["ExecMainExitTimestamp"]); ts != nil {
		s.LastUpgradeAt = ts
		s.LastUpgradeSource = "automatic"
	}
}

func timerState(props map[string]any) updateentity.TimerState {
	active, file := stringProp(props, "ActiveState"), stringProp(props, "UnitFileState")
	if active == "failed" {
		return updateentity.TimerFailed
	}
	if file == "disabled" || file == "masked" {
		return updateentity.TimerDisabled
	}
	if active == "active" {
		return updateentity.TimerActive
	}
	return updateentity.TimerInactive
}
func aggregateTimerState(states []updateentity.TimerState) updateentity.TimerState {
	for _, state := range states {
		if state == updateentity.TimerFailed {
			return state
		}
	}
	for _, state := range states {
		if state == updateentity.TimerDisabled {
			return state
		}
	}
	for _, state := range states {
		if state == updateentity.TimerNotFound {
			return state
		}
	}
	for _, state := range states {
		if state != updateentity.TimerActive {
			return state
		}
	}
	if len(states) > 0 {
		return updateentity.TimerActive
	}
	return updateentity.TimerUnknown
}
func stringProp(props map[string]any, key string) string {
	if value, ok := props[key].(string); ok {
		return value
	}
	return ""
}
func systemdTimestamp(value any) *time.Time {
	var micros uint64
	switch typed := value.(type) {
	case uint64:
		micros = typed
	case int64:
		if typed > 0 {
			micros = uint64(typed)
		}
	}
	if micros == 0 || micros == ^uint64(0) {
		return nil
	}
	t := time.UnixMicro(int64(micros)).UTC()
	return &t
}

func collectPending(ctx context.Context, s *updateentity.Status, executor updateCommandExecutor, opts updateOptions) {
	if _, err := os.Stat("/usr/lib/update-notifier/apt-check"); err == nil {
		output, runErr := runUpdateStep(ctx, executor, opts.timeout, "/usr/lib/update-notifier/apt-check")
		// apt-check may use a non-zero status to signal available updates; parse valid output first.
		if total, security, ok := parseAptCheck(output); ok {
			s.PendingUpdatesTotal, s.PendingSecurityUpdates = &total, &security
			s.DataSources = append(s.DataSources, "apt-check")
			collectEligible(s)
			return
		} else if runErr == nil {
			collectEligible(s)
			return
		}
	}
	output, err := runUpdateStep(ctx, executor, opts.aptTimeout, "apt-get", "-s", "-o", "Debug::NoLocking=true", "upgrade")
	if err != nil {
		s.CollectionErrors = append(s.CollectionErrors, "apt simulation: "+sanitizeUpdateText(err.Error(), 256))
		return
	}
	if count, ok := parseAptSimulation(output); ok {
		s.PendingUpdatesTotal = &count
		s.DataSources = append(s.DataSources, "apt-get-simulation")
	}
	collectEligible(s)
}

func collectEligible(s *updateentity.Status) {
	// unattended-upgrade is not reliably safe as the unprivileged beszel user.
	// The update manager replaces this partial state through the root-owned helper.
	s.CollectionStatus = "partial"
	s.EligibilityStatus = "unavailable"
	s.EligibilityErrorCode = "helper_unavailable"
}

func (m *updateManager) collectPrivilegedEligibility(ctx context.Context, s *updateentity.Status) {
	if s == nil || !s.Supported || s.InstallationState != updateentity.InstallationInstalled {
		return
	}
	check := m.eligibilityCheck()
	if check == nil {
		return
	}
	result := check(ctx)
	if result.status != "available" {
		s.CollectionStatus = "partial"
		s.EligibilityStatus = result.status
		s.EligibilityErrorCode = result.errorCode
		s.EligibilityRetryable = result.retryable
		return
	}
	if count, ok := parseEligibleUpdates(result.output); ok {
		s.PendingUpdatesEligible, s.PendingUpdates = &count, &count
		if s.PendingUpdatesTotal != nil && *s.PendingUpdatesTotal >= count {
			excluded := *s.PendingUpdatesTotal - count
			s.PendingUpdatesExcluded = &excluded
		}
		s.DataSources = append(s.DataSources, "maintenance-helper-dry-run")
		s.EligibilityStatus = "available"
		s.EligibilityErrorCode = ""
		s.EligibilityRetryable = false
		if len(s.CollectionErrors) == 0 {
			s.CollectionStatus = "complete"
		} else {
			s.CollectionStatus = "partial"
		}
		if s.PendingUpdatesExcluded != nil && *s.PendingUpdatesExcluded > 0 {
			s.ExcludedRepositories = parseExcludedRepositories(result.output)
		}
		return
	}
	s.CollectionStatus = "partial"
	s.EligibilityStatus = "unavailable"
	s.EligibilityErrorCode = "invalid_helper_output"
}

func parseExcludedRepositories(output []byte) []string {
	seen := map[string]struct{}{}
	for line := range strings.Lines(string(output)) {
		remaining := line
		for {
			_, after, ok := strings.Cut(remaining, "origin:'")
			if !ok {
				break
			}
			origin, rest, ok := strings.Cut(after, "'")
			if !ok {
				break
			}
			remaining = rest
			origin = sanitizeUpdateText(origin, 80)
			switch strings.ToLower(origin) {
			case "", "debian", "ubuntu", "canonical", "raspbian", "raspberry pi foundation":
				continue
			}
			seen[origin] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for origin := range seen {
		result = append(result, origin)
	}
	sort.Strings(result)
	return result
}

func parseEligibleUpdates(output []byte) (uint16, bool) {
	for line := range strings.Lines(string(output)) {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"Packages that will be upgraded:", "pkgs that look like they should be upgraded:"} {
			if value, ok := strings.CutPrefix(trimmed, prefix); ok {
				value = strings.Trim(value, " []'")
				if value == "" {
					return 0, true
				}
				parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\'' || r == '"' })
				seen := map[string]struct{}{}
				for _, part := range parts {
					part = strings.TrimSpace(part)
					if part != "" {
						seen[part] = struct{}{}
					}
				}
				if len(seen) <= 65535 {
					return uint16(len(seen)), true
				}
			}
		}
	}
	return 0, false
}

func runUpdateStep(parent context.Context, executor updateCommandExecutor, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	output, err := executor.Run(ctx, name, args...)
	if ctx.Err() != nil {
		return output, fmt.Errorf("%s timed out: %w", name, ctx.Err())
	}
	return output, err
}

func parseAptCheck(output []byte) (uint16, uint16, bool) {
	text := strings.TrimSpace(string(output))
	for line := range strings.Lines(text) {
		parts := strings.Split(strings.TrimSpace(line), ";")
		if len(parts) != 2 {
			continue
		}
		total, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 16)
		security, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 16)
		if err1 == nil && err2 == nil && security <= total {
			return uint16(total), uint16(security), true
		}
	}
	return 0, 0, false
}

func parseAptSimulation(output []byte) (uint16, bool) {
	var count uint64
	for line := range strings.Lines(string(output)) {
		if strings.HasPrefix(line, "Inst ") {
			count++
		}
	}
	if count > 65535 {
		return 0, false
	}
	// A successful simulation with no Inst lines reliably means zero pending.
	return uint16(count), true
}

func collectReboot(s *updateentity.Status, maxPackages int) {
	if _, err := os.Stat("/run/reboot-required"); err == nil {
		s.RebootRequired = true
		s.DataSources = append(s.DataSources, "reboot-required")
	}
	if data, err := os.ReadFile("/run/reboot-required.pkgs"); err == nil {
		s.RebootRequiredBy = readPackageLines(data, maxPackages)
	}
}

func collectAptHistory(s *updateentity.Status, maxPackages int) {
	paths := []string{"/var/log/apt/history.log", "/var/log/apt/history.log.1"}
	for _, path := range paths {
		data, err := tailFile(path, maxUpdateLogBytes)
		if err != nil {
			continue
		}
		when, packages, source := parseLatestAptHistory(data, maxPackages)
		if when != nil {
			if s.LastUpgradeAt == nil || when.After(*s.LastUpgradeAt) {
				s.LastUpgradeAt = when
				s.LastUpgradeSource = source
			}
			s.RecentlyUpdatedPackages = packages
			s.DataSources = append(s.DataSources, "apt-history")
			return
		}
	}
}

func tailFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(info.Size()-limit, 0)
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if start > 0 {
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	return data, err
}

func parseLatestAptHistory(data []byte, maxPackages int) (*time.Time, []string, string) {
	blocks := strings.Split(string(data), "\n\n")
	for i := len(blocks) - 1; i >= 0; i-- {
		var when *time.Time
		var packages []string
		source := "manual"
		for line := range strings.Lines(blocks[i]) {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, "Start-Date:"); ok {
				if parsed, err := time.ParseInLocation("2006-01-02  15:04:05", strings.TrimSpace(value), time.Local); err == nil {
					utc := parsed.UTC()
					when = &utc
				}
			}
			if value, ok := strings.CutPrefix(line, "Upgrade:"); ok {
				// Entries contain a comma inside the version tuple, so split only
				// between the closing tuple and the next package.
				for item := range strings.SplitSeq(value, "), ") {
					name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(item), ":", 2)[0])
					if name != "" {
						packages = append(packages, name)
					}
					if len(packages) == maxPackages {
						break
					}
				}
			}
			if value, ok := strings.CutPrefix(line, "Commandline:"); ok && strings.Contains(value, "unattended-upgrade") {
				source = "automatic"
			}
		}
		if when != nil && len(packages) > 0 {
			return when, readPackageLines([]byte(strings.Join(packages, "\n")), maxPackages), source
		}
	}
	return nil, nil, ""
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
