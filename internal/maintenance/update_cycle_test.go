package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

func TestFixedAPTInventoryScriptRunsWithSystemPythonAndFakeModules(t *testing.T) {
	pythonPath, err := exec.LookPath("/usr/bin/python3")
	if err != nil {
		t.Skip("distribution Python is unavailable")
	}
	modules := t.TempDir()
	aptModule := `
class Origin:
    origin = "Debian"
    label = "Debian"
    codename = "bookworm"
    archive = "oldstable"
    site = "deb.debian.org"
    trusted = True
class Version:
    def __init__(self, version): self.version = version
class Candidate:
    def __init__(self, version, priority=500):
        self.version = version
        self.policy_priority = priority
        self.origins = [Origin()]
class Internal:
    def __init__(self, selected_state): self.selected_state = selected_state
class Package:
    def __init__(self, name, held=False, priority=500, versions=None, installed="1.0", candidate="2.0"):
        self.is_installed = True
        self.installed = Version(installed)
        self.candidate = Candidate(candidate, priority)
        self.versions = [Version(v) for v in (versions or [installed, candidate])]
        self._pkg = Internal(2 if held else 1)
class _Cache(dict):
    def __init__(self):
        super().__init__((f"candidate-{i:03d}", Package(f"candidate-{i:03d}")) for i in range(500))
        self["held-package"] = Package("held-package", held=True)
        self["pinned-package"] = Package("pinned-package", priority=-1)
        self["downgrade-only"] = Package("downgrade-only", versions=["0.9"])
        self["epoch-tilde-upgrade"] = Package("epoch-tilde-upgrade", installed="1:2.0~rc1-1", candidate="1:2.0-1")
        self["revision-upgrade"] = Package("revision-upgrade", installed="1.0-1", candidate="1.0-2")
        self["pinned-downgrade"] = Package("pinned-downgrade", installed="2.0", candidate="1.9", versions=["1.9", "2.1"])
def Cache(): return _Cache()
`
	aptPkgModule := `
SELSTATE_HOLD = 2
def version_compare(left, right):
    special = {"1:2.0~rc1-1": 1, "1:2.0-1": 2, "1.0-1": 3, "1.0-2": 4, "1.9": 5, "2.0": 6, "2.1": 7}
    if left in special and right in special:
        return (special[left] > special[right]) - (special[left] < special[right])
    def parts(value): return tuple(int(piece) for piece in value.split("."))
    return (parts(left) > parts(right)) - (parts(left) < parts(right))
`
	if err := os.WriteFile(filepath.Join(modules, "apt.py"), []byte(aptModule), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modules, "apt_pkg.py"), []byte(aptPkgModule), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), pythonPath, "-c", aptInventoryScript)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+modules)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixed inventory script failed with fake python-apt modules: %v: %s", err, output)
	}
	var inventory aptInventory
	if err := json.Unmarshal(output, &inventory); err != nil {
		t.Fatalf("invalid script JSON: %v: %s", err, output)
	}
	if len(output) <= maxCommandOutputBytes {
		t.Fatalf("fixture output is %d bytes and does not exercise the legacy 64 KiB truncation", len(output))
	}
	if len(inventory.Packages) != 505 {
		t.Fatalf("inventory has %d packages, want 505 (downgrade-only package excluded)", len(inventory.Packages))
	}
	states := map[string]inventoryPackage{}
	for _, pkg := range inventory.Packages {
		states[pkg.Name] = pkg
	}
	if _, ok := states["downgrade-only"]; ok {
		t.Fatal("fixed script classified a downgrade as an available update")
	}
	if held := states["held-package"].Held; held == nil || !*held {
		t.Fatalf("hold state was not captured: %#v", states["held-package"])
	}
	if pinned := states["pinned-package"]; pinned.Priority != -1 || len(pinned.Origins) != 1 || pinned.Origins[0].Site != "deb.debian.org" || pinned.Origins[0].Codename != "bookworm" {
		t.Fatalf("pin or origin metadata missing: %#v", pinned)
	}
	if !*states["epoch-tilde-upgrade"].CandidateNewer || !*states["revision-upgrade"].CandidateNewer || *states["pinned-downgrade"].CandidateNewer {
		t.Fatalf("apt_pkg version comparison missed epoch/tilde/revision or pinned downgrade: %#v", states)
	}
	t.Setenv("PYTHONPATH", modules)
	h := testHelper(t)
	h.Runner = ExecRunner{}
	throughRunner, err := h.readAPTInventory(context.Background())
	if err != nil || len(throughRunner.Packages) != 505 {
		t.Fatalf("ExecRunner inventory path truncated a valid large JSON snapshot: packages=%d err=%v", len(throughRunner.Packages), err)
	}
}

func TestFixedAPTInventoryMissingModuleReturnsErrorAndUnknownCount(t *testing.T) {
	modules := t.TempDir()
	if err := os.WriteFile(filepath.Join(modules, "apt.py"), []byte("class Cache: pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modules, "apt_pkg.py"), []byte("raise ModuleNotFoundError(\"No module named 'apt_pkg'\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONPATH", modules)
	h := testHelper(t)
	h.Runner = ExecRunner{}
	inventory, err := h.readAPTInventory(context.Background())
	if err == nil || inventory.Packages != nil {
		t.Fatalf("missing python-apt dependency was reported as a zero inventory: inventory=%#v err=%v", inventory, err)
	}
}

func TestInventoryClassificationDoesNotInventBlockerAndLimitsOnlyDisplayList(t *testing.T) {
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=debian\nVERSION_CODENAME=bookworm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := entity.DefaultPolicy()
	origin := inventoryOrigin{Origin: "Debian", Label: "Debian", Codename: "bookworm", Archive: "oldstable", Site: "deb.debian.org", Trusted: true}
	inventory := aptInventory{Packages: make([]inventoryPackage, 121)}
	installable := make(map[string]bool)
	for i := range inventory.Packages {
		name := fmt.Sprintf("pkg-%03d", i)
		inventory.Packages[i] = inventoryPackage{Name: name, Installed: "1.0", Candidate: "2.0", NewerAvailable: true, CandidateNewer: boolPointer(true), Priority: 500, Held: boolPointer(false), Origins: []inventoryOrigin{origin}}
		if i < 120 {
			installable[name] = true
		}
	}
	result := h.classifyInventory(inventory, policy, installable, nil, true, nil)
	if result.Counts.Candidates != 121 || result.Counts.Eligible != 120 || result.Counts.Unknown != 1 || result.Counts.Blocked != 0 || result.Counts.Pending != 121 || len(result.Counts.Packages) != maxCyclePackageList || result.Valid {
		t.Fatalf("unexpected classification: valid=%v counts=%#v", result.Valid, result.Counts)
	}
	if result.Counts.Packages[0].Name != "pkg-000" {
		t.Fatalf("display list was not stable and sorted: %#v", result.Counts.Packages[:2])
	}
}

func TestPinnedOlderCandidateIsBlockedOnlyWithVersionEvidence(t *testing.T) {
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=debian\nVERSION_CODENAME=bookworm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := entity.DefaultPolicy()
	origin := inventoryOrigin{Origin: "Debian", Label: "Debian", Codename: "bookworm", Archive: "oldstable", Site: "deb.debian.org", Trusted: true}
	inventory := aptInventory{Packages: []inventoryPackage{{Name: "pinned", Installed: "2.0", Candidate: "1.9", NewerAvailable: true, CandidateNewer: boolPointer(false), Priority: 500, Held: boolPointer(false), Origins: []inventoryOrigin{origin}}}}
	result := h.classifyInventory(inventory, policy, nil, nil, true, nil)
	if !result.Valid || result.Counts.Blocked != 1 || result.Counts.Unknown != 0 || result.Counts.Packages[0].State != "blocked" {
		t.Fatalf("newer version evidence did not identify pinned downgrade: valid=%v counts=%#v", result.Valid, result.Counts)
	}
}

func TestInventoryRepositoryAndSecurityClassificationFailClosed(t *testing.T) {
	h := testHelper(t)
	security := entity.DefaultPolicy()
	security.Mode = entity.ModeSecurity
	knownSecurity := inventoryOrigin{Origin: "Debian", Label: "Debian-Security", Codename: "bookworm-security", Archive: "oldstable-security", Site: "security.debian.org", Trusted: true}
	pkg := inventoryPackage{Name: "security-package", Installed: "1.0", Candidate: "2.0", NewerAvailable: true, Priority: 500, Held: boolPointer(false), Origins: []inventoryOrigin{knownSecurity}}
	if allowed, known := candidateAllowed(pkg, security, "bookworm", nil); !allowed || !known {
		t.Fatalf("current Debian security pocket was rejected: allowed=%v known=%v", allowed, known)
	}
	if allowed, known := candidateAllowed(pkg, security, "", nil); allowed || known {
		t.Fatalf("unknown release was treated as a security authorization: allowed=%v known=%v", allowed, known)
	}
	ubuntuSecurity := inventoryOrigin{Origin: "Ubuntu", Label: "Ubuntu", Codename: "jammy", Archive: "jammy-security", Site: "security.ubuntu.com", Trusted: true}
	if allowed, known := candidateAllowed(inventoryPackage{Name: "ubuntu-security", Installed: "1.0", Candidate: "2.0", NewerAvailable: true, Priority: 500, Held: boolPointer(false), Origins: []inventoryOrigin{ubuntuSecurity}}, security, "jammy", nil); !allowed || !known {
		t.Fatalf("Ubuntu codename/base archive security pocket was rejected: allowed=%v known=%v", allowed, known)
	}
	config, err := h.renderUnattendedConfig(security)
	if err != nil || !strings.Contains(string(config), "origin=Ubuntu,codename=${distro_codename},archive=${distro_codename}-security") || strings.Contains(string(config), "origin=Ubuntu,codename=${distro_codename},archive=${distro_codename}-updates") {
		t.Fatalf("security policy does not isolate Ubuntu security archive: err=%v config=%s", err, config)
	}
	first := entity.Repository{ID: "legacy-a", Origin: "Vendor", Label: "Vendor", Codename: "bookworm", Archive: "stable", Site: "vendor.example", Trusted: true}
	second := first
	second.ID, second.Site = "legacy-b", "mirror.example"
	policy := entity.DefaultPolicy()
	policy.Mode = entity.ModeCustom
	policy.AllowedRepositories = []string{first.ID}
	policy.ConfirmThirdParty = true
	customOrigin := inventoryOrigin{Origin: first.Origin, Label: first.Label, Codename: first.Codename, Archive: first.Archive, Site: "vendor.example", Trusted: true}
	customPackage := inventoryPackage{Name: "custom", Installed: "1", Candidate: "2", NewerAvailable: true, Priority: 500, Held: boolPointer(false), Origins: []inventoryOrigin{customOrigin}}
	if allowed, known := candidateAllowed(customPackage, policy, "bookworm", []entity.Repository{first, second}); !allowed || !known {
		t.Fatalf("authorized legacy repository ID/site did not match: allowed=%v known=%v", allowed, known)
	}
	customPackage.Origins[0].Site = "mirror.example"
	if allowed, known := candidateAllowed(customPackage, policy, "bookworm", []entity.Repository{first, second}); allowed || !known {
		t.Fatalf("metadata from a second site inherited authorization: allowed=%v known=%v", allowed, known)
	}
	customPackage.Origins = append(customPackage.Origins, customOrigin)
	if allowed, known := candidateAllowed(customPackage, policy, "bookworm", []entity.Repository{first, second}); !allowed || !known {
		t.Fatalf("an authorized secondary origin was rejected: allowed=%v known=%v", allowed, known)
	}
	_ = h
}

func TestParseInReleaseClearSignedHeaderAndStableAliases(t *testing.T) {
	data := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA512\n\nOrigin: Debian\nLabel: Debian\nSuite: oldstable\nCodename: bookworm\nComponents: main contrib\nArchitectures: amd64\n\n-----BEGIN PGP SIGNATURE-----\nignored\n")
	repo := parseInRelease(data)
	if repo.Origin != "Debian" || repo.Codename != "bookworm" || repo.Archive != "oldstable" || len(repo.Components) != 2 {
		t.Fatalf("clearsigned Release metadata was not parsed: %#v", repo)
	}
	if !isCurrentReleaseOrigin(inventoryOrigin{Codename: "bookworm", Archive: "oldstable"}, "bookworm") || !isCurrentReleaseOrigin(inventoryOrigin{Codename: "bookworm-security"}, "bookworm") {
		t.Fatal("stable aliases or current security pocket were rejected")
	}
	if isCurrentReleaseOrigin(inventoryOrigin{Codename: "bullseye", Archive: "stable"}, "bookworm") {
		t.Fatal("suite alias authorized another release")
	}
	if siteFromListName("archive.ubuntu.com_ubuntu_dists_jammy_InRelease") != "archive.ubuntu.com" || siteFromListName("pkgs_tailscale_com_stable_debian_dists_bookworm_InRelease") != "pkgs.tailscale.com" {
		t.Fatal("repository host normalization lost a real or legacy site")
	}
}

func TestCycleEndToEndUsesOneUnboundedInstallAndReplaysTerminalStatus(t *testing.T) {
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=debian\nVERSION_CODENAME=bookworm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if config, err := h.renderUnattendedConfig(policy); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(h.path("/etc/apt/apt.conf.d/52beszel-plus-unattended-upgrades"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &cyclePipelineRunner{helper: h}
	h.Runner = runner
	request := cycleRequest(policy)
	response := h.Execute(context.Background(), request)
	if response.Status != entity.StateCompleted || response.Result == nil || response.Result.UpdateCycle == nil {
		t.Fatalf("cycle did not complete: %#v", response)
	}
	cycle := response.Result.UpdateCycle
	if cycle.State != entity.CycleCompleted || cycle.InitialCounts == nil || cycle.InitialCounts.Eligible != 120 || len(cycle.InitialCounts.Packages) != maxCyclePackageList {
		t.Fatalf("initial inventory or terminal state incorrect: %#v", cycle)
	}
	installs := 0
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "unattended-upgrade" && len(call) > 1 && call[1] == "--verbose" {
			installs++
			if len(call) != 2 {
				t.Fatalf("install was limited to package names: %#v", call)
			}
		}
	}
	if installs != 1 {
		t.Fatalf("got %d unbounded install invocations, want one: %#v", installs, runner.calls)
	}
	beforeCalls := len(runner.calls)
	replay := h.Execute(context.Background(), request)
	if replay.Status != entity.StateCompleted || replay.Result == nil || replay.Result.UpdateCycle == nil || replay.Result.UpdateCycle.CycleID != request.CycleID || len(runner.calls) != beforeCalls {
		t.Fatalf("terminal cycle replay was not stable: response=%#v calls=%d/%d", replay, len(runner.calls), beforeCalls)
	}
}

func TestCycleFailuresDoNotTreatUnknownInventoryAsSuccess(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if config, err := h.renderUnattendedConfig(policy); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(h.path("/etc/apt/apt.conf.d/52beszel-plus-unattended-upgrades"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	h.Runner = &cyclePipelineRunner{helper: h, unknownAfterDryRun: true}
	response := h.Execute(context.Background(), cycleRequest(policy))
	if response.Status != entity.StateFailed || response.Result == nil || response.Result.UpdateCycle == nil || response.Result.UpdateCycle.State != entity.CycleFailed || response.Result.UpdateCycle.Verified || response.Result.UpdateCycle.Counts == nil || response.Result.UpdateCycle.Counts.Unknown != 1 {
		t.Fatalf("unknown remaining update was reported as success: %#v", response)
	}
}

func TestCycleLockFailureUsesAPTOutputAndPersistsRetryBackoff(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	runner := &failingAPTUpdateRunner{}
	h.Runner = runner
	request := cycleRequest(policy)
	response := h.Execute(context.Background(), request)
	if response.Status != entity.StateFailed || !response.Retryable || response.ErrorCode != "apt_lock_busy" || response.Result == nil || response.Result.UpdateCycle == nil || response.Result.UpdateCycle.State != entity.CycleRetryWait || response.NextAttemptAt == nil {
		t.Fatalf("lock race was not classified from command output: %#v", response)
	}
	if runner.updateCalls != 1 {
		t.Fatalf("apt update calls=%d, want one", runner.updateCalls)
	}
	replay := h.Execute(context.Background(), request)
	if replay.Result == nil || replay.Result.UpdateCycle == nil || replay.Result.UpdateCycle.Attempt != 1 || runner.updateCalls != 1 {
		t.Fatalf("same-cycle request bypassed persisted retry backoff: %#v calls=%d", replay, runner.updateCalls)
	}
}

func TestCycleClaimDoesNotOverwriteActiveCycleAndPolicyChangeCancelsRetry(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	active := &entity.CycleStatus{CycleID: "cycle-00000000000000000001", Source: entity.CycleSourceAutomatic, PolicyRevision: policyRevision(policy), State: entity.CycleRunning}
	store := cycleStore{Watermark: 1, Current: active}
	if err := h.writeCycleStore(store); err != nil {
		t.Fatal(err)
	}
	newCycle := cycleRequest(policy)
	newCycle.CycleID = "cycle-00000000000000000002"
	newCycle.Source = entity.CycleSourceAutomatic
	newCycle.RequestID = "cycle-second"
	second := h.Execute(context.Background(), newCycle)
	if second.ErrorCode != "cycle_in_progress" {
		t.Fatalf("new cycle replaced a nonterminal current cycle: %#v", second)
	}
	store, err := h.readCycleStore()
	if err != nil || store.Watermark != 1 || store.Current == nil || store.Current.CycleID != active.CycleID {
		t.Fatalf("active cycle or watermark changed after rejected claim: store=%#v err=%v", store, err)
	}

	active.State = entity.CycleRetryWait
	active.NextAttemptAt = ptrTime(h.Now().Add(time.Hour))
	if err := h.writeCycleStore(cycleStore{Watermark: 1, Current: active}); err != nil {
		t.Fatal(err)
	}
	changed := policy
	changed.AutomaticReboot = true
	if err := h.writePolicy(changed); err != nil {
		t.Fatal(err)
	}
	h.Runner = &fakeRunner{}
	retry := cycleRequest(policy)
	retry.Source = entity.CycleSourceAutomatic
	retry.RequestID = "cycle-retry-old-revision"
	canceled := h.Execute(context.Background(), retry)
	if canceled.Result == nil || canceled.Result.UpdateCycle == nil || canceled.Result.UpdateCycle.State != entity.CycleCanceled || canceled.Result.UpdateCycle.ErrorCode != "policy_revision_conflict" {
		t.Fatalf("retry under stale policy revision was not canceled: %#v", canceled)
	}
}

func TestCycleStoreReadFailurePreventsAnyAPTCommand(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.cycleFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	h.Runner = runner
	response := h.Execute(context.Background(), cycleRequest(policy))
	if response.Status != entity.StateFailed || response.ErrorCode != "cycle_state_unavailable" || len(runner.calls) != 0 {
		t.Fatalf("cycle state failure did not fail closed: response=%#v calls=%#v", response, runner.calls)
	}
}

func TestLegacySecurityPolicyIsAdoptedOnceAndNewSecurityIsPreserved(t *testing.T) {
	h := testHelper(t)
	legacy := entity.DefaultPolicy()
	legacy.Mode = entity.ModeSecurity
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.stateDir(), "policy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	request := req(entity.AdoptUpdatePolicy)
	request.IdempotencyKey = "adopt-first"
	first := h.Execute(context.Background(), request)
	if first.Status != entity.StateCompleted || first.Result == nil || first.Result.Policy == nil || first.Result.Policy.Mode != entity.ModeOfficialAll {
		t.Fatalf("legacy security policy was not expanded on adoption: %#v", first)
	}
	var document map[string]json.RawMessage
	policyData, err := os.ReadFile(filepath.Join(h.stateDir(), "policy.json"))
	if err != nil || json.Unmarshal(policyData, &document) != nil {
		t.Fatalf("adopted policy metadata unreadable: %v", err)
	}
	var metadata policyMetadata
	if err := json.Unmarshal(document["_beszel"], &metadata); err != nil || metadata.AdoptionVersion != 1 {
		t.Fatalf("policy adoption metadata missing: %#v err=%v", metadata, err)
	}
	newSecurity := entity.DefaultPolicy()
	newSecurity.Mode = entity.ModeSecurity
	if err := h.writePolicy(newSecurity); err != nil {
		t.Fatal(err)
	}
	request.RequestID = "adopt-second"
	request.IdempotencyKey = "adopt-second"
	second := h.Execute(context.Background(), request)
	if second.Status != entity.StateCompleted || second.Result == nil || second.Result.Policy == nil || second.Result.Policy.Mode != entity.ModeSecurity {
		t.Fatalf("explicit new security policy was migrated a second time: %#v", second)
	}
}

func TestExecRunnerDoesNotKillSupervisedCommandOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	startCalled, timeoutCalled := false, false
	output, err, timedOut := (ExecRunner{}).RunSupervised(ctx, "/bin/sh", func(pid int) { startCalled = pid > 1 }, func() { timeoutCalled = true }, "-c", "sleep 0.12; printf child-finished")
	if err != nil || !timedOut || !startCalled || !timeoutCalled || !strings.Contains(string(output), "child-finished") || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("supervised process did not survive client deadline: output=%q err=%v timedOut=%v started=%v timeout=%v elapsed=%v", output, err, timedOut, startCalled, timeoutCalled, time.Since(started))
	}
}

func TestCycleDryRunKeepsCompletePackageListBeyondLegacyOutputLimit(t *testing.T) {
	commandDir := t.TempDir()
	command := "#!/bin/sh\nprintf 'Packages that will be upgraded:\\n'\ni=0\nwhile [ \"$i\" -lt 6000 ]; do printf 'package-%08d ' \"$i\"; i=$((i+1)); done\nprintf '\\n'\n"
	if err := os.WriteFile(filepath.Join(commandDir, "unattended-upgrade"), []byte(command), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commandDir+":"+os.Getenv("PATH"))
	h := testHelper(t)
	h.Runner = ExecRunner{}
	store := cycleStore{Watermark: 1, Current: &entity.CycleStatus{CycleID: "cycle-00000000000000000001", Source: entity.CycleSourceManual, PolicyRevision: strings.Repeat("c", 64), State: entity.CycleRunning}}
	if err := h.writeCycleStore(store); err != nil {
		t.Fatal(err)
	}
	output, _, err := h.cycleCommandWithOutputLimit(context.Background(), &store, time.Second, maxStructuredCommandOutputBytes, string(entity.CycleStageVerify), "unattended-upgrade", "--dry-run", "--debug")
	if err != nil || len(output) <= maxCommandOutputBytes {
		t.Fatalf("dry-run output was truncated at the legacy limit: bytes=%d err=%v", len(output), err)
	}
	installable, _, known := parseUnattendedDryRun(output)
	if !known || len(installable) != 6000 {
		t.Fatalf("large dry-run package list was not fully parsed: known=%v packages=%d", known, len(installable))
	}
}

func TestEffectiveAPTConfigReadsCompleteDumpBeforeComparingPolicy(t *testing.T) {
	policy := entity.DefaultPolicy()
	h := testHelper(t)
	config, err := h.renderUnattendedConfig(policy)
	if err != nil {
		t.Fatal(err)
	}
	var dump strings.Builder
	dump.WriteString(`Unattended-Upgrade::Origins-Pattern:: "origin=Unauthorized,codename=other";` + "\n")
	for range 6000 {
		dump.WriteString("# padding that a legacy tail-only capture would hide\n")
	}
	for _, pattern := range managedPatternValues(config) {
		fmt.Fprintf(&dump, "Unattended-Upgrade::Origins-Pattern:: %q;\n", pattern)
	}
	for _, key := range []string{"Unattended-Upgrade::Automatic-Reboot", "Unattended-Upgrade::Automatic-Reboot-Time", "Unattended-Upgrade::Remove-Unused-Dependencies"} {
		fmt.Fprintf(&dump, "%s %q;\n", key, aptConfigScalar(config, key))
	}
	if dump.Len() <= maxCommandOutputBytes {
		t.Fatalf("fixture dump is too small to expose legacy truncation: %d bytes", dump.Len())
	}
	commandDir := t.TempDir()
	dumpPath := filepath.Join(commandDir, "dump.txt")
	if err := os.WriteFile(dumpPath, []byte(dump.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat '" + dumpPath + "'\n"
	if err := os.WriteFile(filepath.Join(commandDir, "apt-config"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commandDir+":"+os.Getenv("PATH"))
	h.Runner = ExecRunner{}
	if err := h.validateEffectivePolicy(context.Background(), policy); err == nil || !strings.Contains(err.Error(), "Origins-Pattern") {
		t.Fatalf("effective APT override before 64 KiB was missed: %v", err)
	}
}

func boolPointer(value bool) *bool { return &value }

func cycleRequest(policy entity.Policy) entity.Request {
	return entity.Request{Version: entity.ProtocolVersion, RequestID: "cycle-request", Operation: entity.RunUnattendedUpgrades, IdempotencyKey: "cycle-idempotency", CycleID: "cycle-00000000000000000001", Source: entity.CycleSourceManual, PolicyRevision: policyRevision(policy)}
}

type cyclePipelineRunner struct {
	helper             *Helper
	calls              [][]string
	dryRuns            int
	aptInventoryCalls  int
	unknownAfterDryRun bool
}

type failingAPTUpdateRunner struct {
	updateCalls int
}

func (r *failingAPTUpdateRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" {
		return []byte("inactive\n"), errors.New("exit status 3")
	}
	if name == "apt-get" {
		r.updateCalls++
		return []byte("E: Could not get lock /var/lib/dpkg/lock-frontend - open (11: Resource temporarily unavailable)\nUnable to acquire the dpkg frontend lock"), errors.New("exit status 1")
	}
	return nil, fmt.Errorf("unexpected command: %s", name)
}

func (r *cyclePipelineRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	switch name {
	case "apt-get":
		return []byte("Reading package lists... Done\n"), nil
	case "apt-config":
		config, err := r.helper.renderUnattendedConfig(entity.DefaultPolicy())
		if err != nil {
			return nil, err
		}
		return aptConfigDumpForTest(config), nil
	case "dpkg":
		return nil, nil
	case "systemctl":
		return []byte("inactive\n"), errors.New("exit status 3")
	case "unattended-upgrade":
		if len(args) > 0 && args[0] == "--dry-run" {
			if r.dryRuns == 0 {
				r.dryRuns++
				return []byte("Packages that will be upgraded:\n" + packageNames(120)), nil
			}
			r.dryRuns++
			return []byte("No packages found that can be upgraded unattended"), nil
		}
		return []byte("All upgrades installed"), nil
	case "/usr/bin/python3":
		if len(args) < 2 || args[0] != "-c" || args[1] != aptInventoryScript {
			return nil, fmt.Errorf("unexpected inventory invocation: %q", call)
		}
		r.aptInventoryCalls++
		if r.aptInventoryCalls == 1 {
			return []byte(`{"packages":` + packagesJSON(120) + `}`), nil
		}
		if r.unknownAfterDryRun {
			return []byte(`{"packages":[{"name":"unknown-pkg","installed":"1.0","candidate":"2.0","newer_available":true,"candidate_newer":true,"priority":500,"held":false,"origins":[{"origin":"Debian","label":"Debian","codename":"bookworm","archive":"oldstable","site":"deb.debian.org","trusted":true}]}]}`), nil
		}
		return []byte(`{"packages":[]}`), nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", strings.Join(call, " "))
	}
}

func packageNames(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = fmt.Sprintf("pkg-%03d", i)
	}
	return strings.Join(parts, " ")
}

func packagesJSON(count int) string {
	packages := make([]string, count)
	origin := `"origins":[{"origin":"Debian","label":"Debian","codename":"bookworm","archive":"oldstable","site":"deb.debian.org","trusted":true}]`
	for i := range packages {
		name := fmt.Sprintf("pkg-%03d", i)
		packages[i] = fmt.Sprintf(`{"name":%q,"installed":"1.0","candidate":"2.0","newer_available":true,"candidate_newer":true,"priority":500,"held":false,%s}`, name, origin)
	}
	return "[" + strings.Join(packages, ",") + "]"
}

func aptConfigDumpForTest(config []byte) []byte {
	var lines []string
	for _, key := range []string{"Unattended-Upgrade::Origins-Pattern", "Unattended-Upgrade::Allowed-Origins"} {
		inList := false
		for _, line := range strings.Split(string(config), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+" {") {
				if strings.HasSuffix(line, "};") {
					continue
				}
				inList = true
				continue
			}
			if inList && line == "};" {
				inList = false
				continue
			}
			if inList && strings.HasPrefix(line, "\"") {
				lines = append(lines, key+":: "+strings.Trim(strings.TrimSuffix(line, ";"), "\""))
			}
		}
	}
	for _, key := range []string{"Unattended-Upgrade::Automatic-Reboot", "Unattended-Upgrade::Automatic-Reboot-Time", "Unattended-Upgrade::Remove-Unused-Dependencies"} {
		for _, line := range strings.Split(string(config), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+" ") {
				lines = append(lines, line)
			}
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
