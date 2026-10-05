package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const cycleHistoryLimit = 128
const maxCycleStoreBytes = 64 << 20

var cycleRetryBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}

type cycleStore struct {
	Watermark     uint64               `json:"watermark"`
	Current       *entity.CycleStatus  `json:"current,omitempty"`
	History       []entity.CycleStatus `json:"history,omitempty"`
	LastSuccessAt *time.Time           `json:"last_success_at,omitempty"`
	Process       *cycleProcess        `json:"process,omitempty"`
}

type cycleProcess struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
	Command    string `json:"command"`
}

type supervisedRunner interface {
	RunSupervised(context.Context, string, func(int), func(), ...string) ([]byte, error, bool)
}

type limitedCycleRunner interface {
	RunSupervisedLimited(context.Context, int, string, func(int), func(), ...string) ([]byte, error, bool)
}

func cycleID(sequence uint64) string { return fmt.Sprintf("cycle-%020d", sequence) }

func policyRevision(policy entity.Policy) string {
	policy.AllowedRepositories = append([]string(nil), policy.AllowedRepositories...)
	sort.Strings(policy.AllowedRepositories)
	data, _ := json.Marshal(policy)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *Helper) cycleFile() string { return filepath.Join(h.stateDir(), "update-cycles.json") }

func (h *Helper) readCycleStore() (cycleStore, error) {
	data, err := readLimited(h.cycleFile(), maxCycleStoreBytes)
	if os.IsNotExist(err) {
		return cycleStore{}, nil
	}
	if err != nil {
		return cycleStore{}, err
	}
	var store cycleStore
	if err := json.Unmarshal(data, &store); err != nil {
		return cycleStore{}, fmt.Errorf("update cycle state is invalid: %w", err)
	}
	if store.Current != nil {
		sequence, err := parseCycleSequence(store.Current.CycleID)
		if err != nil || sequence > store.Watermark {
			return cycleStore{}, errors.New("update cycle watermark is invalid")
		}
	}
	return store, nil
}

func (h *Helper) writeCycleStore(store cycleStore) error {
	data, err := json.Marshal(store)
	if err != nil {
		return err
	}
	if len(data) > maxCycleStoreBytes {
		return errors.New("update cycle state exceeds its storage limit")
	}
	return atomicWrite(h.cycleFile(), append(data, '\n'), 0o600)
}

func parseCycleSequence(value string) (uint64, error) {
	if len(value) != 26 || !strings.HasPrefix(value, "cycle-") {
		return 0, errors.New("invalid cycle ID")
	}
	return strconv.ParseUint(value[6:], 10, 64)
}

func (h *Helper) updateCycleStatus(req entity.Request) entity.Response {
	store, err := h.readCycleStore()
	if err != nil {
		return failureFor(req, &operationError{code: "cycle_state_unavailable", stage: "cycle_state_read", message: err.Error()})
	}
	var status *entity.CycleStatus
	if req.CycleID == "" {
		status = store.Current
	} else if store.Current != nil && store.Current.CycleID == req.CycleID {
		status = store.Current
	} else {
		for i := len(store.History) - 1; i >= 0; i-- {
			if store.History[i].CycleID == req.CycleID {
				copy := store.History[i]
				status = &copy
				break
			}
		}
	}
	if status == nil && req.CycleID != "" {
		return failureFor(req, &operationError{code: "cycle_expired", stage: "cycle_status", message: "cycle ID is unknown or expired"})
	}
	policy, err := h.readPolicy()
	if err != nil {
		return failureFor(req, &operationError{code: "policy_unavailable", stage: "policy_read", message: err.Error()})
	}
	revision := policyRevision(policy)
	current := cloneCycleStatus(status)
	if current != nil && isTerminalCycle(current.State) {
		current.NextRunAt = cycleNextRunAt(policy, current.FinishedAt)
	}
	result := &entity.Result{UpdateCycle: current, NextCycleID: cycleID(store.Watermark + 1), PolicyRevision: revision, Policy: &policy}
	return h.complete(req, result, false)
}

func cloneCycleStatus(status *entity.CycleStatus) *entity.CycleStatus {
	if status == nil {
		return nil
	}
	copy := *status
	if status.Counts != nil {
		counts := *status.Counts
		counts.Packages = append([]entity.CyclePackage(nil), status.Counts.Packages...)
		copy.Counts = &counts
	}
	if status.InitialCounts != nil {
		counts := *status.InitialCounts
		counts.Packages = append([]entity.CyclePackage(nil), status.InitialCounts.Packages...)
		copy.InitialCounts = &counts
	}
	return &copy
}

func (h *Helper) executeUpdateCycle(ctx context.Context, req entity.Request) entity.Response {
	store, err := h.readCycleStore()
	if err != nil {
		return failureFor(req, &operationError{code: "cycle_state_unavailable", stage: "cycle_state_read", message: err.Error()})
	}
	sequence, err := parseCycleSequence(req.CycleID)
	if err != nil {
		return failureFor(req, &operationError{code: "invalid_cycle_id", stage: "cycle_claim", message: err.Error()})
	}
	if sequence <= store.Watermark {
		if store.Current == nil || store.Current.CycleID != req.CycleID {
			for i := len(store.History) - 1; i >= 0; i-- {
				if store.History[i].CycleID == req.CycleID {
					return cycleResponse(req, &store.History[i], nil)
				}
			}
			return failureFor(req, &operationError{code: "cycle_expired", stage: "cycle_claim", message: "cycle ID was consumed and is no longer replayable"})
		}
		if isTerminalCycle(store.Current.State) {
			return cycleResponse(req, store.Current, nil)
		}
		if store.Current.Source != req.Source || store.Current.PolicyRevision != req.PolicyRevision {
			return failureFor(req, &operationError{code: "cycle_replay_conflict", stage: "cycle_claim", message: "cycle ID was already claimed with different parameters"})
		}
	} else {
		if sequence != store.Watermark+1 {
			return failureFor(req, &operationError{code: "cycle_id_out_of_order", stage: "cycle_claim", message: "cycle IDs must be claimed sequentially"})
		}
		if store.Current != nil && !isTerminalCycle(store.Current.State) {
			return failureFor(req, &operationError{code: "cycle_in_progress", stage: "cycle_claim", retryable: true, message: "another update cycle is still active"})
		}
	}
	policy, err := h.readPolicy()
	if err != nil {
		return failureFor(req, &operationError{code: "policy_unavailable", stage: "policy_read", message: err.Error()})
	}
	currentRevision := policyRevision(policy)
	sameCycle := store.Current != nil && store.Current.CycleID == req.CycleID
	if sameCycle && store.Current.State == entity.CycleRunning && store.Process != nil && processIsAlive(store.Process) {
		store.Current.TimeoutObserved = true
		store.Current.ErrorCode = "timeout_observed"
		store.Current.Error = "APT subprocess is still active; reconciliation is waiting for its exit"
		if currentRevision != req.PolicyRevision || !policy.Enabled || policy.Mode == entity.ModeMonitorOnly {
			store.Current.Error += "; the policy changed while the subprocess was active"
		}
		store.Current.UpdatedAt = ptrTime(h.Now().UTC())
		if err := h.writeCycleStore(store); err != nil {
			return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_reconcile", message: err.Error()})
		}
		return cycleResponse(req, store.Current, nil)
	}
	if !policy.Enabled || policy.Mode == entity.ModeMonitorOnly {
		if store.Current != nil && !isTerminalCycle(store.Current.State) && store.Current.CycleID == req.CycleID {
			store.Current.State = entity.CycleCanceled
			store.Current.Error = "cycle canceled because the update policy is disabled or monitor-only"
			store.Current.ErrorCode = "policy_disabled"
			store.Current.FinishedAt = ptrTime(h.Now().UTC())
			store.Current.UpdatedAt = store.Current.FinishedAt
			if err := h.writeCycleStore(store); err != nil {
				return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
			}
			return cycleResponse(req, store.Current, nil)
		}
		return failureFor(req, &operationError{code: "policy_disabled", stage: "policy_validation", message: "update policy is disabled or monitor-only"})
	}
	if sameCycle {
		if policy.UnattendedUpgradeDays == 0 && store.Current.State == entity.CycleRetryWait {
			store.Current.State = entity.CycleCanceled
			store.Current.Error = "cycle canceled because the automatic update interval is zero"
			store.Current.ErrorCode = "automatic_schedule_disabled"
			store.Current.FinishedAt = ptrTime(h.Now().UTC())
			store.Current.UpdatedAt = store.Current.FinishedAt
			store.Current.NextAttemptAt = nil
			if err := h.writeCycleStore(store); err != nil {
				return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
			}
			return cycleResponse(req, store.Current, nil)
		}
		if store.Current.State == entity.CycleRetryWait && store.Current.NextAttemptAt != nil && h.Now().Before(*store.Current.NextAttemptAt) && currentRevision == req.PolicyRevision {
			return cycleResponse(req, store.Current, nil)
		}
		if store.Current.State != entity.CycleRunning && currentRevision != req.PolicyRevision {
			store.Current.State = entity.CycleCanceled
			store.Current.Error = "cycle canceled because the policy revision changed"
			store.Current.ErrorCode = "policy_revision_conflict"
			store.Current.FinishedAt = ptrTime(h.Now().UTC())
			store.Current.UpdatedAt = store.Current.FinishedAt
			store.Current.NextRunAt = cycleNextRunAt(policy, store.Current.FinishedAt)
			if err := h.writeCycleStore(store); err != nil {
				return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
			}
			return cycleResponse(req, store.Current, nil)
		}
		if store.Current.State == entity.CycleRunning && currentRevision != req.PolicyRevision {
			store.Current.State = entity.CycleCanceled
			store.Current.Error = "cycle canceled because the policy revision changed"
			store.Current.ErrorCode = "policy_revision_conflict"
			store.Current.FinishedAt = ptrTime(h.Now().UTC())
			store.Current.UpdatedAt = store.Current.FinishedAt
			if err := h.writeCycleStore(store); err != nil {
				return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
			}
			return cycleResponse(req, store.Current, nil)
		}
	}
	if currentRevision != req.PolicyRevision {
		return failureFor(req, &operationError{code: "policy_revision_conflict", stage: "policy_validation", message: "update policy changed before cycle claim"})
	}
	if !sameCycle && req.Source == entity.CycleSourceAutomatic && policy.UnattendedUpgradeDays == 0 {
		return failureFor(req, &operationError{code: "automatic_schedule_disabled", stage: "policy_validation", message: "automatic update interval is zero"})
	}
	if h.poweroffStatus(ctx).Scheduled {
		if sameCycle {
			store.Current.ErrorCode = "poweroff_scheduled"
			store.Current.Error = "update cycle is waiting for the scheduled poweroff to finish or be canceled"
			store.Current.UpdatedAt = ptrTime(h.Now().UTC())
			if err := h.writeCycleStore(store); err != nil {
				return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_reconcile", message: err.Error()})
			}
			return cycleResponse(req, store.Current, nil)
		}
		return failureFor(req, &operationError{code: "poweroff_scheduled", stage: "power_guard", retryable: true, message: "update cycle was deferred because a poweroff is scheduled"})
	}
	if !sameCycle {
		store.Watermark = sequence
	}
	now := h.Now().UTC()
	if !sameCycle {
		if store.Current != nil && isTerminalCycle(store.Current.State) {
			store.History = append(store.History, *cloneCycleStatus(store.Current))
			if len(store.History) > cycleHistoryLimit {
				store.History = append([]entity.CycleStatus(nil), store.History[len(store.History)-cycleHistoryLimit:]...)
			}
		}
		store.Current = &entity.CycleStatus{CycleID: req.CycleID, Source: req.Source, PolicyRevision: currentRevision, State: entity.CycleQueued, Attempt: 0, LastSuccessAt: store.LastSuccessAt, UpdatedAt: &now}
	}
	cycle := store.Current
	cycle.Attempt++
	cycle.State = entity.CycleRunning
	cycle.Stage = entity.CycleStageRefresh
	if sameCycle {
		cycle.Stage = entity.CycleStageReconcile
	}
	cycle.Error, cycle.ErrorCode = "", ""
	cycle.NextAttemptAt = nil
	cycle.LastAttemptAt = &now
	cycle.UpdatedAt = &now
	if cycle.StartedAt == nil {
		cycle.StartedAt = &now
	}
	// Claim and every state transition are durable before any package command.
	if err := h.writeCycleStore(store); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_claim", message: err.Error()})
	}
	if err := h.waitForAPT(ctx); err != nil {
		return h.cycleFailure(req, store, aptLockErrorForCycle(err))
	}
	if err := h.cycleSetStage(&store, entity.CycleStageRefresh); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_refresh", message: err.Error()})
	}
	refreshArgs := []string{"-o", "APT::Update::Error-Mode=any", "-o", "DPkg::Lock::Timeout=60", "update"}
	out, timedOut, commandErr := h.cycleCommand(ctx, &store, 20*time.Minute, string(entity.CycleStageRefresh), "apt-get", refreshArgs...)
	if commandErr != nil {
		var typed *operationError
		if errors.As(commandErr, &typed) {
			return h.cycleFailure(req, store, typed)
		}
		return h.cycleFailure(req, store, h.classifyCycleCommandFailure("apt-get", entity.CycleStageRefresh, commandErr))
	}
	if refreshErr := validateRefreshOutput(out); refreshErr != nil {
		return h.cycleFailure(req, store, &operationError{code: "apt_refresh_failed", stage: string(entity.CycleStageRefresh), retryable: isNetworkRefreshFailure(out, refreshErr), message: "APT index refresh failed: " + sanitize(refreshErr.Error(), 512)})
	}
	if err := h.validateEffectivePolicy(ctx, policy); err != nil {
		return h.cycleFailure(req, store, &operationError{code: "policy_effective_conflict", stage: string(entity.CycleStageRefresh), message: err.Error()})
	}
	if err := h.runDpkgAudit(ctx, &store); err != nil {
		return h.cycleFailure(req, store, cycleInventoryError(err))
	}
	if err := h.cycleSetStage(&store, entity.CycleStageVerify); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_inventory", message: err.Error()})
	}
	before, err := h.inventorySnapshot(ctx, &store, policy)
	if err != nil {
		return h.cycleFailure(req, store, cycleInventoryError(err))
	}
	cycle.Counts = &before.Counts
	cycle.InitialCounts = &before.Counts
	cycle.Verified = false
	cycle.VerificationMessage = "pre-install inventory is complete"
	cycle.UpdatedAt = ptrTime(h.Now().UTC())
	if err := h.writeCycleStore(store); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_inventory", message: err.Error()})
	}
	if before.Counts.Eligible > 0 {
		if err := h.cycleSetStage(&store, entity.CycleStageInstall); err != nil {
			return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_install", message: err.Error()})
		}
		_, installTimedOut, installErr := h.cycleCommand(ctx, &store, 2*time.Hour, string(entity.CycleStageInstall), "unattended-upgrade", "--verbose")
		if installErr != nil {
			return h.cycleFailure(req, store, h.classifyCycleCommandFailure("unattended-upgrade", entity.CycleStageInstall, installErr))
		}
		if installTimedOut {
			cycle.TimeoutObserved = true
			cycle.VerificationMessage = "install exceeded its client deadline; subprocess exited and the cycle was reconciled"
		}
	}
	if err := h.cycleSetStage(&store, entity.CycleStageReconcile); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_reconcile", message: err.Error()})
	}
	if err := h.runDpkgAudit(ctx, &store); err != nil {
		return h.cycleFailure(req, store, cycleInventoryError(err))
	}
	if err := h.cycleSetStage(&store, entity.CycleStageVerify); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_verify", message: err.Error()})
	}
	after, err := h.inventorySnapshot(ctx, &store, policy)
	if err != nil {
		return h.cycleFailure(req, store, cycleInventoryError(err))
	}
	cycle.Counts = &after.Counts
	cycle.Verified = after.Valid
	cycle.VerificationMessage = "post-install inventory and dpkg audit completed"
	if timedOut {
		cycle.TimeoutObserved = true
		cycle.VerificationMessage += "; refresh exceeded its client deadline and was reconciled after exit"
	}
	if !after.Valid || after.Counts.Eligible > 0 || after.Counts.Unknown > 0 {
		return h.cycleFailure(req, store, &operationError{code: "verification_incomplete", stage: string(entity.CycleStageVerify), message: "eligible updates remain or package inventory contains unknown results"})
	}
	return h.finishCycle(req, store)
}

func (h *Helper) inventorySnapshot(ctx context.Context, store *cycleStore, policy entity.Policy) (inventoryResult, error) {
	inventory, err := h.readAPTInventory(ctx)
	if err != nil {
		return inventoryResult{}, err
	}
	var repositories []entity.Repository
	if policy.Mode == entity.ModeCustom {
		repositories, err = h.detectRepositories()
		if err != nil {
			return inventoryResult{}, err
		}
	}
	out, timedOut, err := h.cycleCommandWithOutputLimit(ctx, store, 2*time.Minute, maxStructuredCommandOutputBytes, string(entity.CycleStageVerify), "unattended-upgrade", "--dry-run", "--debug")
	if err != nil {
		return inventoryResult{}, &operationError{code: "dry_run_failed", stage: string(entity.CycleStageVerify), retryable: true, message: "unattended-upgrade verification failed: " + sanitize(err.Error(), 512)}
	}
	if timedOut {
		store.Current.TimeoutObserved = true
		store.Current.VerificationMessage = "verification dry-run exceeded its client deadline and exited before parsing"
		store.Current.UpdatedAt = ptrTime(h.Now().UTC())
		if err := h.writeCycleStore(*store); err != nil {
			return inventoryResult{}, &operationError{code: "cycle_state_write_failed", stage: string(entity.CycleStageVerify), message: err.Error()}
		}
	}
	installable, blocked, known := parseUnattendedDryRun(out)
	result := h.classifyInventory(inventory, policy, installable, blocked, known, repositories)
	if !known {
		return result, &operationError{code: "dry_run_unclassified", stage: string(entity.CycleStageVerify), message: "unattended-upgrade dry-run output could not be classified"}
	}
	return result, nil
}

func (h *Helper) validateEffectivePolicy(ctx context.Context, policy entity.Policy) error {
	expectedConfig, err := h.renderUnattendedConfig(policy)
	if err != nil {
		return err
	}
	expectedPatterns := managedPatternValues(expectedConfig)
	output, err := h.command(ctx, time.Minute, "apt-config", "dump")
	if err != nil {
		return &operationError{code: "apt_config_invalid", stage: string(entity.CycleStageRefresh), message: "effective APT configuration could not be read: " + sanitize(err.Error(), 512)}
	}
	values, lists := parseEffectiveAPTConfig(output)
	if len(lists["Unattended-Upgrade::Allowed-Origins"]) != 0 {
		return errors.New("effective Unattended-Upgrade::Allowed-Origins contains entries outside the Beszel policy")
	}
	if !sameStrings(lists["Unattended-Upgrade::Origins-Pattern"], expectedPatterns) {
		return errors.New("effective Unattended-Upgrade::Origins-Pattern differs from the managed policy")
	}
	for _, key := range []string{"Unattended-Upgrade::Automatic-Reboot", "Unattended-Upgrade::Automatic-Reboot-Time", "Unattended-Upgrade::Remove-Unused-Dependencies"} {
		expected := aptConfigScalar(expectedConfig, key)
		if values[key] != expected {
			return fmt.Errorf("effective %s differs from the managed policy", key)
		}
	}
	return nil
}

func managedPatternValues(config []byte) []string {
	values := []string{}
	inPatternList := false
	for line := range strings.Lines(string(config)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Unattended-Upgrade::Origins-Pattern {") {
			inPatternList = true
			continue
		}
		if inPatternList && line == "};" {
			break
		}
		if inPatternList && strings.HasPrefix(line, "\"") {
			if value, _, ok := strings.Cut(strings.TrimPrefix(line, "\""), "\""); ok {
				values = append(values, value)
			}
		}
	}
	return uniqueStrings(values)
}

func parseEffectiveAPTConfig(data []byte) (map[string]string, map[string][]string) {
	values := make(map[string]string)
	lists := make(map[string][]string)
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		index := strings.IndexAny(line, " \t")
		if index < 0 {
			continue
		}
		key := strings.TrimSpace(line[:index])
		value := strings.Trim(strings.TrimSpace(line[index:]), "\"")
		if strings.HasSuffix(key, "::") {
			lists[strings.TrimSuffix(key, "::")] = append(lists[strings.TrimSuffix(key, "::")], value)
		} else {
			values[key] = value
		}
	}
	for key := range lists {
		sort.Strings(lists[key])
	}
	return values, lists
}

func aptConfigScalar(config []byte, key string) string {
	for line := range strings.Lines(string(config)) {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if rest, ok := strings.CutPrefix(line, key+" "); ok {
			return strings.Trim(strings.TrimSpace(rest), "\"")
		}
	}
	return ""
}

func sameStrings(left, right []string) bool {
	left, right = uniqueStrings(left), uniqueStrings(right)
	return len(left) == len(right) && strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func (h *Helper) cycleSetStage(store *cycleStore, stage entity.CycleStage) error {
	now := h.Now().UTC()
	store.Current.Stage = stage
	store.Current.State = entity.CycleRunning
	store.Current.Error, store.Current.ErrorCode = "", ""
	store.Current.UpdatedAt = &now
	return h.writeCycleStore(*store)
}

func (h *Helper) cycleCommand(ctx context.Context, store *cycleStore, timeout time.Duration, stage, name string, args ...string) ([]byte, bool, error) {
	return h.cycleCommandWithOutputLimit(ctx, store, timeout, commandOutputLimit(name, args...), stage, name, args...)
}

func (h *Helper) cycleCommandWithOutputLimit(ctx context.Context, store *cycleStore, timeout time.Duration, outputLimit int, stage, name string, args ...string) ([]byte, bool, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	store.Process = &cycleProcess{Command: name}
	if err := h.writeCycleStore(*store); err != nil {
		store.Process = nil
		return nil, false, &operationError{code: "cycle_state_write_failed", stage: stage, message: err.Error()}
	}
	var callbackPersistErr error
	onStart := func(pid int) {
		process := &cycleProcess{PID: pid, Command: name}
		process.StartTicks, _ = processStartTicks(pid)
		store.Process = process
		if callbackPersistErr == nil {
			callbackPersistErr = h.writeCycleStore(*store)
		}
	}
	onTimeout := func() {
		now := h.Now().UTC()
		store.Current.State = entity.CycleRunning
		store.Current.Stage = entity.CycleStage(stage)
		store.Current.TimeoutObserved = true
		store.Current.Error = "command exceeded its client deadline; waiting for the APT subprocess to exit"
		store.Current.ErrorCode = "timeout_observed"
		store.Current.UpdatedAt = &now
		if callbackPersistErr == nil {
			callbackPersistErr = h.writeCycleStore(*store)
		}
	}
	var output []byte
	var err error
	var timedOut bool
	if runner, ok := h.Runner.(limitedCycleRunner); ok {
		output, err, timedOut = runner.RunSupervisedLimited(stepCtx, outputLimit, name, onStart, onTimeout, args...)
	} else if runner, ok := h.Runner.(supervisedRunner); ok {
		output, err, timedOut = runner.RunSupervised(stepCtx, name, onStart, onTimeout, args...)
	} else {
		output, err = h.Runner.Run(stepCtx, name, args...)
		timedOut = stepCtx.Err() != nil
	}
	store.Process = nil
	persistErr := h.writeCycleStore(*store)
	if callbackPersistErr != nil {
		return output, timedOut, &operationError{code: "cycle_state_write_failed", stage: stage, message: callbackPersistErr.Error()}
	}
	if persistErr != nil {
		return output, timedOut, &operationError{code: "cycle_state_write_failed", stage: stage, message: persistErr.Error()}
	}
	if err != nil {
		detail := err.Error()
		if text := sanitize(string(output), 512); text != "" {
			detail += ": " + text
		}
		return output, timedOut, h.classifyCycleCommandFailure(name, entity.CycleStage(stage), errors.New(detail))
	}
	return output, timedOut, nil
}

func (h *Helper) classifyCycleCommandFailure(name string, stage entity.CycleStage, err error) *operationError {
	text := strings.ToLower(err.Error())
	result := &operationError{code: "command_failed", stage: string(stage), message: fmt.Sprintf("%s failed: %s", name, sanitize(err.Error(), 512))}
	switch {
	case errors.Is(err, exec.ErrNotFound), strings.Contains(text, "not found"):
		result.code = "command_missing"
	case strings.Contains(text, "timed out"), errors.Is(err, context.DeadlineExceeded):
		result.code, result.retryable = "timeout", true
	case aptLockBusy([]byte(err.Error())):
		result.code, result.retryable = "apt_lock_busy", true
	case strings.Contains(text, "temporary failure resolving"), strings.Contains(text, "could not resolve"), strings.Contains(text, "failed to fetch"), strings.Contains(text, "connection timed out"), strings.Contains(text, "network is unreachable"):
		result.code, result.retryable = "network_error", true
	}
	if result.code == "command_failed" {
		if lock, err := h.aptActivity(); err == nil && lock != nil {
			result.code, result.retryable = "apt_lock_busy", true
			result.lock = lock
		}
	}
	return result
}

func (h *Helper) cycleFailure(req entity.Request, store cycleStore, cause *operationError) entity.Response {
	cycle := store.Current
	now := h.Now().UTC()
	cycle.Error, cycle.ErrorCode = sanitize(cause.message, 512), cause.code
	cycle.UpdatedAt = &now
	cycle.Stage = entity.CycleStage(cause.stage)
	if cause.retryable && h.cycleIntervalEnabled() {
		cycle.State = entity.CycleRetryWait
		index := min(int(cycle.Attempt)-1, len(cycleRetryBackoff)-1)
		cycle.NextAttemptAt = ptrTime(now.Add(cycleRetryBackoff[index]))
	} else {
		cycle.State = entity.CycleFailed
		cycle.FinishedAt = &now
		cycle.NextAttemptAt = nil
		policy, policyErr := h.readPolicy()
		if policyErr == nil {
			cycle.NextRunAt = cycleNextRunAt(policy, &now)
		}
	}
	if err := h.writeCycleStore(store); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
	}
	return cycleResponse(req, cycle, cause)
}

func (h *Helper) finishCycle(req entity.Request, store cycleStore) entity.Response {
	cycle := store.Current
	now := h.Now().UTC()
	cycle.FinishedAt = &now
	cycle.LastSuccessAt = &now
	store.LastSuccessAt = &now
	cycle.NextAttemptAt = nil
	if policy, err := h.readPolicy(); err == nil {
		cycle.NextRunAt = cycleNextRunAt(policy, &now)
	}
	cycle.Verified = true
	if cycle.Counts != nil && cycle.Counts.Held+cycle.Counts.Excluded+cycle.Counts.Blocked > 0 {
		cycle.State = entity.CycleCompletedWithPending
		cycle.VerificationMessage = "all remaining updates have an explicit hold, exclusion, or blocker reason"
	} else {
		cycle.State = entity.CycleCompleted
		cycle.VerificationMessage = "no eligible updates remain"
	}
	cycle.UpdatedAt = &now
	if err := h.writeCycleStore(store); err != nil {
		return failureFor(req, &operationError{code: "cycle_state_write_failed", stage: "cycle_persist", message: err.Error()})
	}
	return cycleResponse(req, cycle, nil)
}

func cycleResponse(req entity.Request, cycle *entity.CycleStatus, cause *operationError) entity.Response {
	result := &entity.Result{UpdateCycle: cloneCycleStatus(cycle)}
	if cause != nil {
		response := failureFor(req, cause)
		response.Result = result
		response.Stage = cause.stage
		response.Retryable = cycle.State == entity.CycleRetryWait
		response.NextAttemptAt = cycle.NextAttemptAt
		return response
	}
	if cycle.State == entity.CycleRunning || cycle.State == entity.CycleRetryWait {
		return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateRunning, Message: cycle.Error, Stage: string(cycle.Stage), Result: result, IdempotencyKey: req.IdempotencyKey, StartedAt: cycle.StartedAt, NextAttemptAt: cycle.NextAttemptAt}
	}
	if cycle.State == entity.CycleFailed || cycle.State == entity.CycleCanceled {
		return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateFailed, Error: cycle.Error, ErrorCode: cycle.ErrorCode, Stage: string(cycle.Stage), Result: result, IdempotencyKey: req.IdempotencyKey, StartedAt: cycle.StartedAt, FinishedAt: cycle.FinishedAt}
	}
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Progress: 100, Result: result, IdempotencyKey: req.IdempotencyKey, StartedAt: cycle.StartedAt, FinishedAt: cycle.FinishedAt}
}

func isTerminalCycle(state entity.CycleState) bool {
	return state == entity.CycleCompleted || state == entity.CycleCompletedWithPending || state == entity.CycleFailed || state == entity.CycleCanceled
}

func ptrTime(value time.Time) *time.Time { return &value }

func (h *Helper) cycleIntervalEnabled() bool {
	policy, err := h.readPolicy()
	return err == nil && policy.UnattendedUpgradeDays > 0
}

func cycleNextRunAt(policy entity.Policy, finished *time.Time) *time.Time {
	if finished == nil || !policy.Enabled || policy.Mode == entity.ModeMonitorOnly || policy.UnattendedUpgradeDays == 0 {
		return nil
	}
	next := finished.Add(time.Duration(policy.UnattendedUpgradeDays) * 24 * time.Hour)
	return &next
}

func aptLockErrorForCycle(err error) *operationError {
	var typed *operationError
	if errors.As(err, &typed) {
		copy := *typed
		copy.stage = string(entity.CycleStageReconcile)
		copy.retryable = true
		return &copy
	}
	return &operationError{code: "apt_lock_busy", stage: string(entity.CycleStageReconcile), retryable: true, message: "APT lock wait failed: " + sanitize(err.Error(), 512)}
}

func processIsAlive(process *cycleProcess) bool {
	if process == nil || process.PID <= 1 {
		return false
	}
	if err := syscall.Kill(process.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if process.StartTicks != 0 {
		ticks, err := processStartTicks(process.PID)
		if err != nil || ticks != process.StartTicks {
			return false
		}
	}
	command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", process.PID))
	if err != nil {
		return false
	}
	cmd := strings.ReplaceAll(string(command), "\x00", " ")
	return strings.Contains(cmd, "apt-get") || strings.Contains(cmd, "unattended-upgrade")
}

func processStartTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	end := strings.LastIndex(string(data), ")")
	if end < 0 || end+2 >= len(data) {
		return 0, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) <= 19 {
		return 0, errors.New("process start time missing")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
