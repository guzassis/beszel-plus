//go:build linux

package agent

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

type updateExecutorFunc func(context.Context, string, ...string) ([]byte, error)

func (fn updateExecutorFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return fn(ctx, name, args...)
}

func TestParseAptConfig(t *testing.T) {
	values := parseAptConfig([]byte("// ignored\nAPT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"0\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"))
	if values["APT::Periodic::Update-Package-Lists"] != "1" || values["APT::Periodic::Unattended-Upgrade"] != "1" {
		t.Fatalf("unexpected values: %#v", values)
	}
	if !aptConfigEnabled("1") || aptConfigEnabled("0") || !aptConfigEnabled("true") {
		t.Fatal("enabled parsing failed")
	}
}

func TestParsePendingUpdates(t *testing.T) {
	total, security, ok := parseAptCheck([]byte("5;2\n"))
	if !ok || total != 5 || security != 2 {
		t.Fatalf("got %d;%d, %v", total, security, ok)
	}
	if _, _, ok = parseAptCheck([]byte("2;5")); ok {
		t.Fatal("accepted security count greater than total")
	}
	count, ok := parseAptSimulation([]byte("Inst curl [1] (2 repo)\nInst openssl [1] (2 repo)\n"))
	if !ok || count != 2 {
		t.Fatalf("simulation count = %d, %v", count, ok)
	}
}

func TestParseEligibleUpdates(t *testing.T) {
	count, ok := parseEligibleUpdates([]byte("Packages that will be upgraded: ['curl', 'openssl', 'linux-image']\n"))
	if !ok || count != 3 {
		t.Fatalf("eligible=%d ok=%v", count, ok)
	}
}

func TestParseExcludedRepositories(t *testing.T) {
	output := []byte("Checking: curl ([<Origin origin:'Debian' site:'deb.debian.org'>])\nChecking: tailscale ([<Origin origin:'Tailscale' site:'pkgs.tailscale.com'>])\n")
	repos := parseExcludedRepositories(output)
	if len(repos) != 1 || repos[0] != "Tailscale" {
		t.Fatalf("repos=%#v", repos)
	}
}

func TestPackageDetectionTimeoutRemainsUnknown(t *testing.T) {
	executor := updateExecutorFunc(func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() })
	status := &updateentity.Status{InstallationState: updateentity.InstallationUnknown}
	collectInstallation(context.Background(), status, executor, time.Millisecond)
	if status.InstallationState == updateentity.InstallationNotInstalled {
		t.Fatalf("timeout became %s", status.InstallationState)
	}
	if len(status.CollectionErrors) == 0 {
		t.Fatal("timeout stage was not reported")
	}
}

func TestPackageDetectionInstalledAndAbsent(t *testing.T) {
	installed := &updateentity.Status{InstallationState: updateentity.InstallationUnknown}
	collectInstallation(context.Background(), installed, updateExecutorFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("installed\n"), nil
	}), time.Second)
	if installed.InstallationState != updateentity.InstallationInstalled {
		t.Fatalf("installed package reported as %s", installed.InstallationState)
	}

	absent := &updateentity.Status{InstallationState: updateentity.InstallationUnknown}
	collectInstallation(context.Background(), absent, updateExecutorFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, &exec.ExitError{}
	}), time.Second)
	if absent.InstallationState != updateentity.InstallationNotInstalled {
		t.Fatalf("absent package reported as %s", absent.InstallationState)
	}
}

func TestFallbackLeavesSecurityCountUnknown(t *testing.T) {
	executor := updateExecutorFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "apt-get" {
			return []byte("Inst curl [1] (2 repo)\n"), nil
		}
		return nil, errors.New("dry-run unavailable")
	})
	status := &updateentity.Status{}
	collectPending(context.Background(), status, executor, updateOptions{timeout: time.Second, aptTimeout: time.Second})
	if status.PendingUpdatesTotal == nil || *status.PendingUpdatesTotal != 1 || status.PendingSecurityUpdates != nil {
		t.Fatalf("fallback counts are misleading: %#v", status)
	}
	if status.CollectionStatus != "partial" || status.EligibilityErrorCode != "helper_unavailable" {
		t.Fatalf("unprivileged eligibility was not isolated: %#v", status)
	}
	if status.LastError != "" || len(status.CollectionErrors) != 0 {
		t.Fatalf("partial eligibility became a global collection error: %#v", status)
	}
}

func TestEligibilityFailurePreservesLastSuccessfulValue(t *testing.T) {
	last := uint16(3)
	checked := time.Unix(1_700_000_000, 0).UTC()
	previous := &updateentity.Status{PendingUpdatesEligible: &last, LastEligibilityCheck: &checked, EligibilityStatus: "available"}
	current := &updateentity.Status{CollectionStatus: "partial", EligibilityStatus: "busy", EligibilityErrorCode: "apt_lock_busy", EligibilityRetryable: true}
	mergeEligibility(current, previous, checked.Add(time.Hour))
	if current.PendingUpdatesEligible == nil || *current.PendingUpdatesEligible != last || current.EligibilityStatus != "stale" || current.LastEligibilityCheck == nil || !current.LastEligibilityCheck.Equal(checked) {
		t.Fatalf("last eligibility result was lost: %#v", current)
	}
}

func TestPrivilegedEligibilitySuccessAndBusyState(t *testing.T) {
	m := &updateManager{}
	m.setEligibilityCheck(func(context.Context) eligibilityCheckResult {
		return eligibilityCheckResult{status: "available", output: []byte("Packages that will be upgraded: ['curl', 'openssl']\n")}
	})
	total := uint16(3)
	status := &updateentity.Status{Supported: true, InstallationState: updateentity.InstallationInstalled, PendingUpdatesTotal: &total}
	m.collectPrivilegedEligibility(context.Background(), status)
	if status.PendingUpdatesEligible == nil || *status.PendingUpdatesEligible != 2 || status.CollectionStatus != "complete" || status.EligibilityStatus != "available" {
		t.Fatalf("helper eligibility result was not applied: %#v", status)
	}
	m.setEligibilityCheck(func(context.Context) eligibilityCheckResult {
		return eligibilityCheckResult{status: "busy", errorCode: "apt_lock_busy", retryable: true}
	})
	status = &updateentity.Status{Supported: true, InstallationState: updateentity.InstallationInstalled, PendingUpdatesTotal: &total}
	m.collectPrivilegedEligibility(context.Background(), status)
	if status.CollectionStatus != "partial" || status.EligibilityStatus != "busy" || !status.EligibilityRetryable {
		t.Fatalf("temporary helper failure became a global failure: %#v", status)
	}
}

func TestCollectionAndMaintenanceAreSerialized(t *testing.T) {
	m := &updateManager{gate: make(chan struct{}, 1)}
	if !m.beginCollection() || m.beginCollection() {
		t.Fatal("overlapping collection was not rejected")
	}
	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		acquired <- m.beginMaintenance(ctx)
	}()
	time.Sleep(10 * time.Millisecond)
	if !m.maintenanceIsActive() || m.beginCollection() {
		t.Fatal("maintenance did not block a new collection")
	}
	m.endCollection()
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	m.endMaintenance()
	if m.maintenanceIsActive() || !m.beginCollection() {
		t.Fatal("coordination state was not released")
	}
	m.endCollection()
}

func TestCollectionRetryUsesBoundedBackoffAndJitter(t *testing.T) {
	first := updateCollectionDelay(6*time.Hour, 1, 1)
	second := updateCollectionDelay(6*time.Hour, 2, 1)
	capped := updateCollectionDelay(20*time.Minute, 7, 1)
	if first < 5*time.Minute || first > 6*time.Minute || second < 10*time.Minute || second > 11*time.Minute {
		t.Fatalf("unexpected retry backoff: first=%v second=%v", first, second)
	}
	if capped < 20*time.Minute || capped > 22*time.Minute {
		t.Fatalf("retry backoff exceeded configured interval: %v", capped)
	}
}

func TestPendingTimeoutPreservesPartialSnapshot(t *testing.T) {
	executor := updateExecutorFunc(func(ctx context.Context, name string, _ ...string) ([]byte, error) {
		if name == "apt-get" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, errors.New("unavailable")
	})
	status := &updateentity.Status{Supported: true, InstallationState: updateentity.InstallationInstalled, ConfigurationState: updateentity.ConfigurationEnabled, TimerState: updateentity.TimerActive}
	collectPending(context.Background(), status, executor, updateOptions{timeout: time.Millisecond, aptTimeout: time.Millisecond})
	if status.InstallationState != updateentity.InstallationInstalled || status.PendingUpdatesTotal != nil {
		t.Fatalf("partial snapshot lost: %#v", status)
	}
	if len(status.CollectionErrors) == 0 {
		t.Fatal("APT timeout was not reported")
	}
}

func TestParseLatestAptHistoryLimitsAndDeduplicates(t *testing.T) {
	data := []byte("Start-Date: 2026-07-10  04:18:00\nUpgrade: old:amd64 (1, 2)\n\nStart-Date: 2026-07-11  04:18:00\nUpgrade: curl:amd64 (1, 2), openssl:amd64 (1, 2), curl:amd64 (1, 2)\nEnd-Date: 2026-07-11  04:18:02\n")
	when, packages, source := parseLatestAptHistory(data, 2)
	if when == nil || when.Year() != 2026 || len(packages) != 2 || packages[0] != "curl" || packages[1] != "openssl" {
		t.Fatalf("got %v %#v", when, packages)
	}
	if source != "manual" {
		t.Fatalf("source=%q", source)
	}
}

func TestParseLatestAptHistoryDetectsUnattendedUpgrade(t *testing.T) {
	data := []byte("Start-Date: 2026-07-11  03:12:00\nCommandline: /usr/bin/unattended-upgrade\nUpgrade: openssl:amd64 (1, 2)\nEnd-Date: 2026-07-11  03:12:02\n")
	_, _, source := parseLatestAptHistory(data, 2)
	if source != "automatic" {
		t.Fatalf("source=%q", source)
	}
}

func TestUpdateSnapshotCopiesCache(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Second)
	m := &updateManager{status: nil, lastAttempt: now}
	if got := m.snapshot(); got == nil || got.OverallState != "monitoring_incomplete" {
		t.Fatalf("unexpected initial snapshot: %#v", got)
	}
}

func TestManualUpgradeMarkerPersistsWithoutSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-monitor-state.json")
	want := time.Unix(1_700_000_000, 0).UTC()
	m := &updateManager{statePath: path}
	m.markManualUpgrade(want)
	restored := &updateManager{statePath: path}
	restored.loadState()
	if restored.persisted.LastManualUpgradeAt == nil || !restored.persisted.LastManualUpgradeAt.Equal(want) {
		t.Fatalf("manual upgrade marker not persisted: %#v", restored.persisted)
	}
}

func TestSecurityAndRebootTimesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-monitor-state.json")
	wantSecurity := time.Unix(1_700_000_100, 0).UTC()
	wantReboot := time.Unix(1_700_000_200, 0).UTC()
	m := &updateManager{
		statePath: path,
		status:    &updateentity.Status{SecurityUpdatesSince: &wantSecurity, RebootRequiredSince: &wantReboot},
	}
	m.saveState()
	restored := &updateManager{statePath: path}
	restored.loadState()
	if restored.persisted.SecurityUpdatesSince == nil || !restored.persisted.SecurityUpdatesSince.Equal(wantSecurity) ||
		restored.persisted.RebootRequiredSince == nil || !restored.persisted.RebootRequiredSince.Equal(wantReboot) {
		t.Fatalf("update alert times not persisted: %#v", restored.persisted)
	}
}
