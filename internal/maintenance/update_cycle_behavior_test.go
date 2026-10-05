package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

type scenarioRunner struct {
	base          cyclePipelineRunner
	refresh       []byte
	refreshErr    error
	audit         []byte
	before, after *aptInventory
	afterDryRun   []byte
	onCommand     func(string)
}

func (r *scenarioRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.base.Run(ctx, name, args...)
	if r.onCommand != nil {
		r.onCommand(name)
	}
	switch name {
	case "apt-get":
		if r.refresh != nil || r.refreshErr != nil {
			return r.refresh, r.refreshErr
		}
	case "apt-config":
		policy, readErr := r.base.helper.readPolicy()
		if readErr != nil {
			return nil, readErr
		}
		config, renderErr := r.base.helper.renderUnattendedConfig(policy)
		return aptConfigDumpForTest(config), renderErr
	case "dpkg":
		if r.audit != nil {
			return r.audit, nil
		}
	case "/usr/bin/python3":
		inventory := r.before
		if r.base.aptInventoryCalls > 1 {
			inventory = r.after
		}
		if inventory != nil {
			return json.Marshal(inventory)
		}
	case "unattended-upgrade":
		if len(args) > 0 && args[0] == "--dry-run" && r.base.dryRuns > 1 && r.afterDryRun != nil {
			return r.afterDryRun, nil
		}
	}
	return out, err
}

func cycleScenario(t *testing.T, policy entity.Policy) (*Helper, *scenarioRunner) {
	t.Helper()
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=debian\nVERSION_CODENAME=bookworm\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	runner := &scenarioRunner{base: cyclePipelineRunner{helper: h}}
	h.Runner = runner
	return h, runner
}

func installCalls(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) > 1 && call[0] == "unattended-upgrade" && call[1] == "--verbose" {
			count++
		}
	}
	return count
}

func TestCycleRefreshFailuresStopBeforeInstallation(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		err          error
		retry        bool
	}{
		{"partial indexes exit zero", "W: Some index files failed to download. Failed to fetch mirror", nil, true},
		{"network exit zero", "Temporary failure resolving deb.debian.org", nil, true},
		{"signature exit zero", "W: GPG error: signatures couldn't be verified: NO_PUBKEY", nil, false},
		{"network exit nonzero", "E: Failed to fetch: Network is unreachable", errors.New("exit status 100"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := entity.DefaultPolicy()
			h, runner := cycleScenario(t, policy)
			runner.refresh, runner.refreshErr = []byte(tc.output), tc.err
			response := h.Execute(context.Background(), cycleRequest(policy))
			cycle := responseCycleForTest(t, response)
			if response.Status != entity.StateFailed || response.Retryable != tc.retry || cycle.Stage != entity.CycleStageRefresh || cycle.LastSuccessAt != nil || installCalls(runner.base.calls) != 0 || runner.base.aptInventoryCalls != 0 {
				t.Fatalf("refresh failure advanced the cycle: %#v calls=%v", response, runner.base.calls)
			}
		})
	}
}

func responseCycleForTest(t *testing.T, response entity.Response) *entity.CycleStatus {
	t.Helper()
	if response.Result == nil || response.Result.UpdateCycle == nil {
		t.Fatalf("missing cycle: %#v", response)
	}
	return response.Result.UpdateCycle
}

func TestCycleZeroAndJustifiedPendingUpdateLastSuccess(t *testing.T) {
	var pending aptInventory
	if err := json.Unmarshal([]byte(`{"packages":`+packagesJSON(2)+`}`), &pending); err != nil {
		t.Fatal(err)
	}
	pending.Packages[0].Held = boolPointer(true)
	pending.Packages[1].Origins[0].Origin = "Vendor"
	pending.Packages[1].Origins[0].Label = "Vendor"
	pending.Packages[1].Origins[0].Site = "vendor.example"
	for _, tc := range []struct {
		name          string
		before, after *aptInventory
		state         entity.CycleState
		installs      int
	}{
		{"zero candidates", &aptInventory{Packages: []inventoryPackage{}}, &aptInventory{Packages: []inventoryPackage{}}, entity.CycleCompleted, 0},
		{"held and excluded remain", nil, &pending, entity.CycleCompletedWithPending, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := entity.DefaultPolicy()
			h, runner := cycleScenario(t, policy)
			runner.before, runner.after = tc.before, tc.after
			cycle := responseCycleForTest(t, h.Execute(context.Background(), cycleRequest(policy)))
			if cycle.State != tc.state || !cycle.Verified || cycle.LastSuccessAt == nil || !cycle.LastSuccessAt.Equal(h.Now()) || installCalls(runner.base.calls) != tc.installs {
				t.Fatalf("unexpected completion: %#v", cycle)
			}
			if tc.state == entity.CycleCompletedWithPending && (cycle.Counts.Held != 1 || cycle.Counts.Excluded != 1 || cycle.Counts.Eligible != 0 || cycle.Counts.Pending != 2) {
				t.Fatalf("pending reasons lost: %#v", cycle.Counts)
			}
			for _, pkg := range cycle.Counts.Packages {
				if pkg.Reason == "" {
					t.Fatalf("pending package lacks reason: %#v", pkg)
				}
			}
		})
	}
}

func TestCycleFailedAttemptPreservesPreviousSuccessAndPersistsBackoff(t *testing.T) {
	policy := entity.DefaultPolicy()
	h, runner := cycleScenario(t, policy)
	first := responseCycleForTest(t, h.Execute(context.Background(), cycleRequest(policy)))
	lastSuccess := *first.LastSuccessAt
	now := lastSuccess.Add(time.Hour)
	h.Now = func() time.Time { return now }
	runner.refresh, runner.refreshErr = []byte("Failed to fetch: network is unreachable"), errors.New("exit status 100")
	request := cycleRequest(policy)
	request.CycleID = cycleID(2)
	for attempt, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour} {
		// A fresh Helper instance must resume exclusively from durable state.
		h = &Helper{Root: h.Root, Runner: runner, Now: func() time.Time { return now }}
		request.IdempotencyKey = fmt.Sprintf("cycle-retry-%d", attempt)
		cycle := responseCycleForTest(t, h.Execute(context.Background(), request))
		if cycle.State != entity.CycleRetryWait || cycle.Attempt != uint32(attempt+1) || cycle.NextAttemptAt == nil || cycle.NextAttemptAt.Sub(now) != delay || cycle.LastSuccessAt == nil || !cycle.LastSuccessAt.Equal(lastSuccess) {
			t.Fatalf("backoff/success changed: %#v", cycle)
		}
		now = *cycle.NextAttemptAt
	}
}

func TestCycleRestartReconcilesEveryStageWithoutReinstalling(t *testing.T) {
	for _, stage := range []entity.CycleStage{entity.CycleStageRefresh, entity.CycleStageInstall, entity.CycleStageVerify, entity.CycleStageReconcile} {
		t.Run(string(stage), func(t *testing.T) {
			policy := entity.DefaultPolicy()
			policy.AutomaticReboot = true
			h, runner := cycleScenario(t, policy)
			runner.before, runner.after = &aptInventory{Packages: []inventoryPackage{}}, &aptInventory{Packages: []inventoryPackage{}}
			request := cycleRequest(policy)
			if err := h.writeCycleStore(cycleStore{Watermark: 1, Current: &entity.CycleStatus{CycleID: request.CycleID, Source: request.Source, PolicyRevision: request.PolicyRevision, State: entity.CycleRunning, Stage: stage, Attempt: 1}}); err != nil {
				t.Fatal(err)
			}
			h = &Helper{Root: h.Root, Runner: runner, Now: h.Now}
			cycle := responseCycleForTest(t, h.Execute(context.Background(), request))
			if cycle.State != entity.CycleCompleted || cycle.Attempt != 2 || !cycle.Verified || installCalls(runner.base.calls) != 0 {
				t.Fatalf("restart repeated installation: %#v calls=%v", cycle, runner.base.calls)
			}
		})
	}
}

func TestCycleLiveSubprocessBlocksReplayAndRevocationWaitsForExit(t *testing.T) {
	policy := entity.DefaultPolicy()
	h, runner := cycleScenario(t, policy)
	child := exec.Command("/bin/bash", "-c", "exec -a apt-get /bin/sleep 30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	process := &cycleProcess{PID: child.Process.Pid, Command: "apt-get"}
	process.StartTicks, _ = processStartTicks(process.PID)
	deadline := time.Now().Add(time.Second)
	for !processIsAlive(process) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !processIsAlive(process) {
		t.Fatal("test subprocess did not initialize")
	}
	reusedPID := *process
	reusedPID.StartTicks++
	if processIsAlive(&reusedPID) {
		t.Fatal("PID with a different start time was accepted")
	}
	request := cycleRequest(policy)
	if err := h.writeCycleStore(cycleStore{Watermark: 1, Process: process, Current: &entity.CycleStatus{CycleID: request.CycleID, Source: request.Source, PolicyRevision: request.PolicyRevision, State: entity.CycleRunning, Stage: entity.CycleStageInstall, Attempt: 1}}); err != nil {
		t.Fatal(err)
	}
	policy.Enabled = false
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	cycle := responseCycleForTest(t, h.Execute(context.Background(), request))
	if cycle.State != entity.CycleRunning || !cycle.TimeoutObserved || len(runner.base.calls) != 0 {
		t.Fatalf("live subprocess replay executed commands: %#v", cycle)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	cycle = responseCycleForTest(t, h.Execute(context.Background(), request))
	if cycle.State != entity.CycleCanceled || len(runner.base.calls) != 0 {
		t.Fatalf("revoked policy resumed installation: %#v", cycle)
	}
}

func TestCycleDpkgAuditAndPersistenceFailurePreventInstall(t *testing.T) {
	for _, failure := range []string{"dpkg", "persist"} {
		t.Run(failure, func(t *testing.T) {
			policy := entity.DefaultPolicy()
			h, runner := cycleScenario(t, policy)
			if failure == "dpkg" {
				runner.audit = []byte("package is half configured")
			} else {
				runner.onCommand = func(name string) {
					if name == "apt-get" {
						if err := os.Remove(h.cycleFile()); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(h.cycleFile(), 0o700); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			response := h.Execute(context.Background(), cycleRequest(policy))
			if response.Status != entity.StateFailed || installCalls(runner.base.calls) != 0 {
				t.Fatalf("failed audit/persistence permitted install: %#v", response)
			}
			if failure == "persist" && response.ErrorCode != "cycle_state_write_failed" {
				t.Fatalf("wrong persistence error: %#v", response)
			}
		})
	}
}

func TestCycleHistoryRemainsReadableAndExpiredIDsCannotExecute(t *testing.T) {
	policy := entity.DefaultPolicy()
	h, runner := cycleScenario(t, policy)
	counts := &entity.CycleCounts{Candidates: 100, Pending: 100, Held: 100}
	for i := 0; i < 100; i++ {
		counts.Packages = append(counts.Packages, entity.CyclePackage{Name: fmt.Sprintf("held-%03d", i), State: "held", Reason: strings.Repeat("held reason ", 20)})
	}
	store := cycleStore{Watermark: 129}
	for id := uint64(1); id <= 129; id++ {
		status := entity.CycleStatus{CycleID: cycleID(id), Source: entity.CycleSourceManual, PolicyRevision: policyRevision(policy), State: entity.CycleCompletedWithPending, Counts: counts, Verified: true}
		if id == 129 {
			store.Current = &status
		} else {
			store.History = append(store.History, status)
		}
	}
	if err := h.writeCycleStore(store); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(h.cycleFile())
	if err != nil || info.Size() <= 1<<20 {
		t.Fatalf("fixture does not exceed old read limit: %v %v", info, err)
	}
	request := cycleRequest(policy)
	request.CycleID = cycleID(130)
	if response := h.Execute(context.Background(), request); response.Status != entity.StateCompleted {
		t.Fatalf("large history prevented next cycle: %#v", response)
	}
	store, err = h.readCycleStore()
	if err != nil || len(store.History) != 128 || store.Watermark != 130 {
		t.Fatalf("history/watermark invalid: len=%d watermark=%d err=%v", len(store.History), store.Watermark, err)
	}
	before := len(runner.base.calls)
	request.CycleID = cycleID(1)
	if response := h.Execute(context.Background(), request); response.ErrorCode != "cycle_expired" || len(runner.base.calls) != before {
		t.Fatalf("expired cycle ran: %#v", response)
	}
	request.CycleID = cycleID(2)
	if response := h.Execute(context.Background(), request); response.Status != entity.StateCompleted || len(runner.base.calls) != before {
		t.Fatalf("retained history replay ran: %#v", response)
	}
}

func TestCycleScheduleFollowsPolicyIntervalAndZeroKeepsManualOnly(t *testing.T) {
	policy := entity.DefaultPolicy()
	h, runner := cycleScenario(t, policy)
	completed := responseCycleForTest(t, h.Execute(context.Background(), cycleRequest(policy)))
	policy.UnattendedUpgradeDays = 7
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	status := responseCycleForTest(t, h.Execute(context.Background(), req(entity.GetUpdateCycleStatus)))
	if status.NextRunAt == nil || status.NextRunAt.Sub(*completed.FinishedAt) != 7*24*time.Hour {
		t.Fatalf("interval change did not recalculate schedule: %#v", status)
	}
	policy.UnattendedUpgradeDays = 0
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	status = responseCycleForTest(t, h.Execute(context.Background(), req(entity.GetUpdateCycleStatus)))
	if status.NextRunAt != nil {
		t.Fatal("zero interval retained automatic schedule")
	}
	request := cycleRequest(policy)
	request.CycleID = cycleID(2)
	request.Source = entity.CycleSourceAutomatic
	before := len(runner.base.calls)
	if response := h.Execute(context.Background(), request); response.ErrorCode != "automatic_schedule_disabled" || len(runner.base.calls) != before {
		t.Fatalf("zero interval permitted auto cycle: %#v", response)
	}
	request.Source = entity.CycleSourceManual
	runner.refreshErr = errors.New("network is unreachable")
	cycle := responseCycleForTest(t, h.Execute(context.Background(), request))
	if cycle.State != entity.CycleFailed || cycle.NextAttemptAt != nil {
		t.Fatalf("manual cycle scheduled auto retry at zero interval: %#v", cycle)
	}
}

func TestAdoptionPreservesExplicitModesAndUnmanagedMachines(t *testing.T) {
	for _, mode := range []entity.PolicyMode{entity.ModeMonitorOnly, entity.ModeCustom, entity.ModeSecurity} {
		t.Run(string(mode), func(t *testing.T) {
			h := testHelper(t)
			policy := entity.DefaultPolicy()
			policy.Mode = mode
			if mode == entity.ModeSecurity {
				policy.Enabled = false
			}
			if mode == entity.ModeCustom {
				policy.AllowedRepositories = []string{"repo-1"}
				policy.ConfirmThirdParty = true
			}
			data, _ := json.Marshal(policy)
			if err := os.WriteFile(filepath.Join(h.stateDir(), "policy.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			response := h.Execute(context.Background(), req(entity.AdoptUpdatePolicy))
			if response.Status != entity.StateCompleted || response.Result == nil || !reflect.DeepEqual(*response.Result.Policy, policy) {
				t.Fatalf("explicit legacy choice changed: %#v", response)
			}
		})
	}
	h := testHelper(t)
	response := h.Execute(context.Background(), req(entity.AdoptUpdatePolicy))
	if response.Status != entity.StateCompleted || response.Result.Policy.Enabled {
		t.Fatalf("unmanaged host enabled: %#v", response)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir(), "policy.json")); !os.IsNotExist(err) {
		t.Fatalf("unmanaged policy created: %v", err)
	}
}

func TestPendingPolicyCannotResurrectSupersededChoice(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	target := policy
	target.AutomaticReboot = true
	if err := h.savePendingPolicy(entity.Response{Result: &entity.Result{Policy: &target}}); err != nil {
		t.Fatal(err)
	}
	policy.Enabled = false
	if err := h.writePolicy(policy); err != nil {
		t.Fatal(err)
	}
	response := h.Execute(context.Background(), req(entity.GetOperationStatus))
	if response.ErrorCode != "pending_policy_superseded" || response.Retryable {
		t.Fatalf("superseded retry remained active: %#v", response)
	}
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &target
	original := entity.DefaultPolicy()
	request.ExpectedPolicyRevision = policyRevision(original)
	response = h.Execute(context.Background(), request)
	if response.ErrorCode != "policy_revision_conflict" {
		t.Fatalf("stale policy applied: %#v", response)
	}
}
