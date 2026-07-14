package maintenance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/henrygd/beszel/internal/aptstatus"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

const maxIPCRequestBytes = 64 * 1024
const maxCommandOutputBytes = 64 * 1024

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if len(output) > maxCommandOutputBytes {
		output = output[len(output)-maxCommandOutputBytes:]
	}
	return output, err
}

type Helper struct {
	Root              string
	Runner            Runner
	Now               func() time.Time
	RunSystemCommands bool
	APTLockTimeout    time.Duration
	APTRetryBase      time.Duration
}

func NewHelper() *Helper {
	return &Helper{Root: "/", Runner: ExecRunner{}, Now: func() time.Time { return time.Now().UTC() }, RunSystemCommands: true, APTLockTimeout: 75 * time.Second, APTRetryBase: 2 * time.Second}
}

func (h *Helper) path(value string) string {
	if h.Root == "" || h.Root == "/" {
		return value
	}
	return filepath.Join(h.Root, strings.TrimPrefix(value, "/"))
}

func (h *Helper) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(input, maxIPCRequestBytes+1))
	if err != nil || len(data) > maxIPCRequestBytes {
		return h.writeResponse(output, failed(entity.Request{}, "request payload too large"))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req entity.Request
	if err := decoder.Decode(&req); err != nil {
		return h.writeResponse(output, failed(req, "invalid request payload"))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return h.writeResponse(output, failed(req, "multiple request values are not allowed"))
	}
	if err := entity.ValidateRequest(req); err != nil {
		return h.writeResponse(output, failed(req, err.Error()))
	}
	response := h.Execute(ctx, req)
	return h.writeResponse(output, response)
}

func (h *Helper) writeResponse(output io.Writer, response entity.Response) error {
	return json.NewEncoder(output).Encode(response)
}

func failed(req entity.Request, message string) entity.Response {
	now := time.Now().UTC()
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateFailed, Error: sanitize(message, 1024), IdempotencyKey: req.IdempotencyKey, FinishedAt: &now}
}

type operationError struct {
	code, stage, message string
	retryable            bool
	rollback             *entity.Rollback
	lock                 *aptLockInfo
}

func (e *operationError) Error() string { return e.message }

func failureFor(req entity.Request, err error) entity.Response {
	response := failed(req, err.Error())
	var typed *operationError
	if errors.As(err, &typed) {
		response.ErrorCode = typed.code
		response.Stage = typed.stage
		response.Retryable = typed.retryable
		response.Rollback = typed.rollback
		if typed.lock != nil {
			response.LockFile = typed.lock.File
			response.HolderPID = typed.lock.PID
			response.HolderCommand = typed.lock.Command
			response.HolderUnit = typed.lock.Unit
		}
	}
	return response
}

func (h *Helper) Execute(ctx context.Context, req entity.Request) entity.Response {
	if err := entity.ValidateRequest(req); err != nil {
		slog.Warn("maintenance operation refused", "operation", req.Operation, "request_id", req.RequestID)
		return failed(req, err.Error())
	}
	slog.Info("maintenance operation requested", "operation", req.Operation, "request_id", req.RequestID)
	if err := h.migrateLegacyBackups(); err != nil {
		return failureFor(req, &operationError{code: "filesystem_write_failed", stage: "legacy_backup_migration", message: err.Error()})
	}
	if cached, ok := h.cachedResponse(req.IdempotencyKey); ok {
		if cached.Operation != req.Operation {
			return failed(req, "idempotency key reused for another operation")
		}
		return cached
	}
	if req.Operation == entity.GetCapabilities {
		return h.complete(req, &entity.Result{Capabilities: h.capabilities()}, false)
	}
	if req.Operation == entity.GetPowerCapabilities {
		return h.complete(req, &entity.Result{PowerCapabilities: h.powerCapabilities()}, false)
	}
	if req.Operation == entity.GetPoweroffStatus {
		status := h.poweroffStatus(ctx)
		return h.complete(req, &entity.Result{PoweroffStatus: &status}, false)
	}
	if req.Operation == entity.GetOperationStatus {
		if pending, err := h.readPendingPolicy(); err == nil {
			return pending
		}
		if status, err := h.readStatus(); err == nil {
			return status
		}
		return failed(req, "operation status unavailable")
	}
	if caps := h.capabilities(); !caps.UpdateManagement {
		return failed(req, "unsupported platform")
	}
	lock, err := h.lock()
	if err != nil {
		return failed(req, "another maintenance operation is running")
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
	start := h.Now()
	running := entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateRunning, Progress: 5, IdempotencyKey: req.IdempotencyKey, StartedAt: &start}
	_ = h.saveStatus(running)
	result, changed, runErr := h.run(ctx, req)
	var response entity.Response
	if runErr != nil {
		response = failureFor(req, runErr)
		response.StartedAt = &start
		if req.Operation == entity.ApplyUpdatePolicy && response.Retryable {
			response.Result = &entity.Result{Policy: req.Policy}
		}
	} else {
		response = h.complete(req, result, changed)
		response.StartedAt = &start
	}
	if req.Operation == entity.ApplyUpdatePolicy {
		if response.Status == entity.StateFailed && response.Retryable && response.Result != nil && response.Result.Policy != nil {
			if h.savePendingPolicy(response) == nil {
				if pending, err := h.readPendingPolicyData(); err == nil {
					response.NextAttemptAt = &pending.NextAttemptAt
				}
			}
		} else {
			_ = os.Remove(filepath.Join(h.stateDir(), "pending-policy.json"))
		}
	}
	_ = h.saveStatus(response)
	duration := h.Now().Sub(start)
	slog.Info("maintenance operation finished", "operation", req.Operation, "request_id", req.RequestID, "status", response.Status, "error_code", response.ErrorCode, "stage", response.Stage, "retryable", response.Retryable, "rollback_attempted", response.Rollback != nil && response.Rollback.Attempted, "rollback_succeeded", response.Rollback != nil && response.Rollback.Succeeded, "duration", duration, "helper_version", HelperVersion(), "protocol_version", entity.ProtocolVersion)
	return response
}

type pendingPolicy struct {
	Policy        entity.Policy `json:"policy"`
	CreatedAt     time.Time     `json:"created_at"`
	LastAttemptAt time.Time     `json:"last_attempt_at"`
	AttemptCount  uint32        `json:"attempt_count"`
	LastErrorCode string        `json:"last_error_code"`
	NextAttemptAt time.Time     `json:"next_attempt_at"`
}

var pendingBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}

func (h *Helper) savePendingPolicy(response entity.Response) error {
	if response.Result == nil || response.Result.Policy == nil {
		return errors.New("pending policy is missing")
	}
	now := h.Now()
	pending := pendingPolicy{Policy: *response.Result.Policy, CreatedAt: now, LastAttemptAt: now, AttemptCount: 1, LastErrorCode: response.ErrorCode}
	if previous, err := h.readPendingPolicyData(); err == nil {
		pending.CreatedAt = previous.CreatedAt
		pending.AttemptCount = previous.AttemptCount + 1
	}
	index := min(int(pending.AttemptCount)-1, len(pendingBackoff)-1)
	pending.NextAttemptAt = now.Add(pendingBackoff[index])
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(h.stateDir(), "pending-policy.json"), append(data, '\n'), 0o600)
}

func (h *Helper) readPendingPolicy() (entity.Response, error) {
	pending, err := h.readPendingPolicyData()
	if err != nil {
		return entity.Response{}, err
	}
	return entity.Response{Version: entity.ProtocolVersion, RequestID: "pending-policy", Operation: entity.ApplyUpdatePolicy, Status: entity.StateFailed, Error: "policy application is pending", ErrorCode: pending.LastErrorCode, Retryable: true, Result: &entity.Result{Policy: &pending.Policy}, FinishedAt: &pending.LastAttemptAt, NextAttemptAt: &pending.NextAttemptAt}, nil
}

func (h *Helper) readPendingPolicyData() (pendingPolicy, error) {
	data, err := readLimited(filepath.Join(h.stateDir(), "pending-policy.json"), 64*1024)
	if err != nil {
		return pendingPolicy{}, err
	}
	var pending pendingPolicy
	err = json.Unmarshal(data, &pending)
	return pending, err
}

func (h *Helper) run(ctx context.Context, req entity.Request) (*entity.Result, bool, error) {
	switch req.Operation {
	case entity.GetUpdatePolicy:
		policy, err := h.readPolicy()
		return &entity.Result{Policy: &policy}, false, err
	case entity.DetectRepositories:
		repos, err := h.detectRepositories()
		return &entity.Result{Repositories: repos}, false, err
	case entity.ValidateUpdatePolicy:
		if err := h.validatePolicy(*req.Policy); err != nil {
			return nil, false, err
		}
		return &entity.Result{Policy: req.Policy, Output: "policy valid"}, false, nil
	case entity.ApplyUpdatePolicy:
		return h.applyPolicy(ctx, req)
	case entity.InstallUpdateDependencies:
		return h.installDependencies(ctx)
	case entity.RunUpdateDryRun:
		out, err := h.unattendedDryRun(ctx)
		return &entity.Result{Output: sanitize(string(out), maxCommandOutputBytes)}, false, err
	case entity.RunUnattendedUpgrades:
		if err := h.waitForAPT(ctx); err != nil {
			return nil, false, err
		}
		out, err := h.command(ctx, 2*time.Hour, "unattended-upgrade", "--verbose")
		return &entity.Result{Output: sanitize(string(out), maxCommandOutputBytes)}, false, err
	case entity.SchedulePoweroff:
		return h.schedulePoweroff(ctx, req.DelaySeconds)
	case entity.CancelPoweroff:
		return h.cancelPoweroff(ctx)
	}
	return nil, false, errors.New("operation not allowed")
}

func (h *Helper) capabilities() *entity.Capabilities {
	platform := platformID(h.path("/etc/os-release"))
	supported := platform == "debian" || platform == "ubuntu" || platform == "raspbian"
	return &entity.Capabilities{UpdateMonitoring: true, UpdateManagement: supported, PrivilegedHelper: true, PolicyRead: supported, PolicyWrite: supported, DryRun: supported, RunUpgrade: supported, AutomaticReboot: supported, SupportedPlatform: platform, HelperVersion: HelperVersion(), ProtocolVersion: entity.ProtocolVersion, MinAgentVersion: MinAgentVersion, MaxProtocolVersion: entity.ProtocolVersion, BuildCommit: BuildCommit, Power: h.powerCapabilities()}
}

func (h *Helper) powerCapabilities() *powerentity.Capabilities {
	return &powerentity.Capabilities{PowerManagement: true, Shutdown: true, CancelShutdown: true, MaxDelaySeconds: 604800}
}

func (h *Helper) schedulePoweroff(ctx context.Context, delay uint32) (*entity.Result, bool, error) {
	if lock := h.aptActivity(); lock != nil {
		return nil, false, &operationError{code: "apt_lock_busy", stage: "power_guard", retryable: true, message: "APT is currently in use", lock: lock}
	}
	if status := h.poweroffStatus(ctx); status.Scheduled {
		return nil, false, &operationError{code: "power_operation_active", stage: "power_guard", message: "a shutdown is already scheduled"}
	}
	if output, err := h.Runner.Run(ctx, "systemd-inhibit", "--list", "--no-pager", "--no-legend"); err == nil && strings.Contains(strings.ToLower(string(output)), "block") {
		return nil, false, &operationError{code: "shutdown_inhibited", stage: "power_guard", message: "shutdown is blocked by a system inhibitor"}
	}
	args := []string{"--unit=beszel-poweroff", "--collect", fmt.Sprintf("--on-active=%ds", max(delay, 3)), "/usr/bin/systemctl", "poweroff"}
	if _, err := h.command(ctx, 10*time.Second, "systemd-run", args...); err != nil {
		return nil, false, &operationError{code: "schedule_failed", stage: "power_schedule", message: err.Error()}
	}
	now := h.Now().Add(time.Duration(max(delay, 3)) * time.Second)
	return &entity.Result{Changed: true, PoweroffStatus: &powerentity.ShutdownStatus{Scheduled: true, ScheduledAt: &now, DelaySeconds: delay}}, true, nil
}

func (h *Helper) cancelPoweroff(ctx context.Context) (*entity.Result, bool, error) {
	_, _ = h.command(ctx, 10*time.Second, "systemctl", "stop", "beszel-poweroff.timer", "beszel-poweroff.service")
	_, _ = h.command(ctx, 10*time.Second, "systemctl", "reset-failed", "beszel-poweroff.timer", "beszel-poweroff.service")
	return &entity.Result{Changed: true, PoweroffStatus: &powerentity.ShutdownStatus{}}, true, nil
}

func (h *Helper) poweroffStatus(ctx context.Context) powerentity.ShutdownStatus {
	out, err := h.Runner.Run(ctx, "systemctl", "is-active", "beszel-poweroff.timer")
	return powerentity.ShutdownStatus{Scheduled: err == nil && strings.TrimSpace(string(out)) == "active"}
}

func platformID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(data)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "ID="); ok {
			return strings.ToLower(strings.Trim(value, "\"'"))
		}
	}
	return ""
}

func (h *Helper) validatePolicy(policy entity.Policy) error {
	if err := entity.ValidatePolicy(policy); err != nil {
		return err
	}
	if policy.Mode != entity.ModeCustom {
		return nil
	}
	repos, err := h.detectRepositories()
	if err != nil {
		return err
	}
	byID := make(map[string]entity.Repository, len(repos))
	for _, repo := range repos {
		byID[repo.ID] = repo
	}
	for _, id := range policy.AllowedRepositories {
		repo, ok := byID[id]
		if !ok {
			return errors.New("selected repository was not detected")
		}
		if !repo.Official && !policy.ConfirmThirdParty {
			return errors.New("third-party repository requires explicit confirmation")
		}
	}
	return nil
}

func (h *Helper) applyPolicy(ctx context.Context, req entity.Request) (*entity.Result, bool, error) {
	policy := *req.Policy
	if err := h.validatePolicy(policy); err != nil {
		return nil, false, err
	}
	unattended, err := h.renderUnattendedConfig(policy)
	if err != nil {
		return nil, false, err
	}
	paths := []string{h.path("/etc/apt/apt.conf.d/20auto-upgrades"), h.path("/etc/apt/apt.conf.d/52beszel-plus-unattended-upgrades")}
	backups := make([][]byte, len(paths))
	existed := make([]bool, len(paths))
	for i, path := range paths {
		backups[i], err = os.ReadFile(path)
		existed[i] = err == nil
		if err != nil && !os.IsNotExist(err) {
			return nil, false, err
		}
	}
	contents := [][]byte{mergeAutoConfig(backups[0], policy), unattended}
	policyPath := filepath.Join(h.stateDir(), "policy.json")
	policyBackup, policyReadErr := os.ReadFile(policyPath)
	policyExisted := policyReadErr == nil
	if policyReadErr != nil && !os.IsNotExist(policyReadErr) {
		return nil, false, policyReadErr
	}
	runSystemCommands := h.Root == "" || h.Root == "/" || h.RunSystemCommands
	var aptDailyEnabled, aptUpgradeEnabled *bool
	if runSystemCommands {
		aptDailyEnabled = h.unitEnabled(ctx, "apt-daily.timer")
		aptUpgradeEnabled = h.unitEnabled(ctx, "apt-daily-upgrade.timer")
	}
	rollback := func() bool {
		slog.Warn("maintenance policy rollback", "operation", req.Operation, "request_id", req.RequestID)
		rollbackCtx := context.WithoutCancel(ctx)
		succeeded := true
		for i, path := range paths {
			if existed[i] {
				if atomicWrite(path, backups[i], 0o644) != nil {
					succeeded = false
				}
			} else {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					succeeded = false
				}
			}
		}
		if policyExisted {
			if atomicWrite(policyPath, policyBackup, 0o600) != nil {
				succeeded = false
			}
		} else {
			if err := os.Remove(policyPath); err != nil && !os.IsNotExist(err) {
				succeeded = false
			}
		}
		if runSystemCommands {
			h.restoreUnit(rollbackCtx, "apt-daily.timer", aptDailyEnabled)
			h.restoreUnit(rollbackCtx, "apt-daily-upgrade.timer", aptUpgradeEnabled)
		}
		return succeeded
	}
	for i, path := range paths {
		if err := h.backupFile(path); err != nil {
			rollback()
			return nil, false, err
		}
		if err := atomicWrite(path, contents[i], 0o644); err != nil {
			rollback()
			return nil, false, err
		}
	}
	if runSystemCommands {
		if _, err := h.command(ctx, time.Minute, "apt-config", "dump"); err != nil {
			rollbackSucceeded := rollback()
			return nil, false, &operationError{code: "apt_config_invalid", stage: "apt_config_validation", message: fmt.Sprintf("apt-config validation failed: %v", err), rollback: &entity.Rollback{Attempted: true, Succeeded: rollbackSucceeded}}
		}
		if _, err := h.unattendedDryRun(ctx); err != nil {
			rollbackSucceeded := rollback()
			var typed *operationError
			if errors.As(err, &typed) {
				typed.rollback = &entity.Rollback{Attempted: true, Succeeded: rollbackSucceeded}
				return nil, false, typed
			}
			return nil, false, &operationError{code: "apt_dry_run_failed", stage: "unattended_upgrade_dry_run", message: fmt.Sprintf("unattended-upgrade dry-run failed: %v", err), rollback: &entity.Rollback{Attempted: true, Succeeded: rollbackSucceeded}}
		}
		if policy.Enabled && policy.Mode != entity.ModeMonitorOnly {
			_, err = h.command(ctx, time.Minute, "systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer")
		} else {
			_, err = h.command(ctx, time.Minute, "systemctl", "disable", "--now", "apt-daily-upgrade.timer")
		}
		if err != nil {
			rollbackSucceeded := rollback()
			return nil, false, &operationError{code: "timer_update_failed", stage: "timer_update", message: fmt.Sprintf("timer update failed: %v", err), rollback: &entity.Rollback{Attempted: true, Succeeded: rollbackSucceeded}}
		}
	}
	if err := h.writePolicy(policy); err != nil {
		rollback()
		return nil, false, err
	}
	return &entity.Result{Policy: &policy, Changed: true, Output: "policy applied"}, true, nil
}

func (h *Helper) unitEnabled(ctx context.Context, name string) *bool {
	stepCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	output, _ := h.Runner.Run(stepCtx, "systemctl", "is-enabled", name)
	switch strings.TrimSpace(string(output)) {
	case "enabled", "enabled-runtime", "static":
		value := true
		return &value
	case "disabled", "masked", "not-found":
		value := false
		return &value
	default:
		return nil
	}
}

func (h *Helper) restoreUnit(ctx context.Context, name string, enabled *bool) {
	if enabled == nil {
		return
	}
	action := "disable"
	if *enabled {
		action = "enable"
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, _ = h.Runner.Run(stepCtx, "systemctl", action, "--now", name)
}

func (h *Helper) installDependencies(ctx context.Context) (*entity.Result, bool, error) {
	if h.Root != "" && h.Root != "/" {
		return nil, false, errors.New("dependency installation unavailable in test root")
	}
	if err := h.waitForAPT(ctx); err != nil {
		return nil, false, err
	}
	if _, err := h.command(ctx, 20*time.Minute, "apt-get", "update"); err != nil {
		return nil, false, fmt.Errorf("apt metadata refresh failed: %w", err)
	}
	out, err := h.command(ctx, 30*time.Minute, "apt-get", "install", "-y", "unattended-upgrades")
	if err != nil {
		return nil, false, fmt.Errorf("dependency installation failed: %w", err)
	}
	if _, err = h.command(ctx, time.Minute, "systemctl", "enable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer"); err != nil {
		return nil, false, fmt.Errorf("timer enable failed: %w", err)
	}
	return &entity.Result{Changed: true, Output: sanitize(string(out), maxCommandOutputBytes)}, true, nil
}

func (h *Helper) command(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := h.Runner.Run(stepCtx, name, args...)
	if stepCtx.Err() != nil {
		return output, fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		return output, fmt.Errorf("%s failed: %s", name, sanitize(string(output), 1024))
	}
	return output, nil
}

func (h *Helper) unattendedDryRun(ctx context.Context) ([]byte, error) {
	timeout := h.APTLockTimeout
	if timeout <= 0 {
		timeout = 75 * time.Second
	}
	base := h.APTRetryBase
	if base <= 0 {
		base = 2 * time.Second
	}
	started := time.Now()
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &operationError{code: "apt_lock_busy", stage: "unattended_upgrade_dry_run", retryable: true, message: "APT remained busy until the installation timeout", lock: h.aptActivity()}
		}
		stepCtx, cancel := context.WithTimeout(ctx, remaining)
		output, err := h.Runner.Run(stepCtx, "unattended-upgrade", "--dry-run", "--debug")
		stepErr := stepCtx.Err()
		cancel()
		if err == nil {
			return output, nil
		}
		if stepErr != nil && !aptLockBusy(output) {
			return output, &operationError{code: "timeout", stage: "unattended_upgrade_dry_run", retryable: true, message: "unattended-upgrade dry-run timed out"}
		}
		if !aptLockBusy(output) {
			return output, classifyDryRunFailure(output, err)
		}
		lock := h.aptActivity()
		wait := base * time.Duration(attempt+1)
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		if !time.Now().Add(wait).Before(deadline) {
			return output, &operationError{code: "apt_lock_busy", stage: "unattended_upgrade_dry_run", retryable: true, message: "APT remained busy until the installation timeout", lock: lock}
		}
		attributes := []any{"stage", "unattended_upgrade_dry_run", "retry_in", wait, "attempt", attempt + 1, "elapsed", time.Since(started), "remaining", time.Until(deadline)}
		if lock != nil {
			attributes = append(attributes, "lock_file", lock.File, "holder_pid", lock.PID, "holder_command", lock.Command, "holder_unit", lock.Unit)
		}
		slog.Info("waiting for APT lock", attributes...)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return output, ctx.Err()
		case <-timer.C:
		}
	}
}

func classifyDryRunFailure(output []byte, err error) *operationError {
	text := strings.ToLower(string(output) + " " + err.Error())
	result := &operationError{code: "apt_dry_run_failed", stage: "unattended_upgrade_dry_run", message: "unattended-upgrade dry-run failed: " + sanitize(string(output), 1024)}
	switch {
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(text, "executable file not found") || strings.Contains(text, "not found"):
		result.code = "command_missing"
	case strings.Contains(text, "permission denied") || strings.Contains(text, "must be root") || strings.Contains(text, "operation not permitted"):
		result.code = "permission_denied"
	case strings.Contains(text, "syntax error") || strings.Contains(text, "invalid configuration") || strings.Contains(text, "error in file"):
		result.code = "invalid_configuration"
	}
	return result
}

func (h *Helper) waitForAPT(ctx context.Context) error {
	timeout := h.APTLockTimeout
	if timeout <= 0 {
		timeout = 75 * time.Second
	}
	base := h.APTRetryBase
	if base <= 0 {
		base = 2 * time.Second
	}
	return h.waitForAPTUntil(ctx, time.Now().Add(timeout), base)
}

func (h *Helper) waitForAPTUntil(ctx context.Context, deadline time.Time, base time.Duration) error {
	started := time.Now()
	for attempt := 0; ; attempt++ {
		lock := h.aptActivity()
		if lock == nil {
			return nil
		}
		wait := base * time.Duration(attempt+1)
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		if !time.Now().Add(wait).Before(deadline) {
			return &operationError{code: "apt_lock_busy", stage: "apt_lock_wait", retryable: true, message: "APT is currently in use", lock: lock}
		}
		slog.Info("waiting for APT lock", "lock_file", lock.File, "holder_pid", lock.PID, "holder_command", lock.Command, "retry_in", wait, "attempt", attempt+1, "elapsed", time.Since(started), "remaining", time.Until(deadline))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type aptLockInfo struct {
	File          string
	PID           int
	Command, Unit string
}

func (h *Helper) aptActivity() *aptLockInfo {
	status, err := aptstatus.Inspect(h.Root)
	if err != nil || !status.Busy || len(status.Holders) == 0 {
		return nil
	}
	holder := status.Holders[0]
	file := ""
	if len(holder.Locks) > 0 {
		file = holder.Locks[0]
	}
	return &aptLockInfo{File: file, PID: holder.PID, Command: sanitize(holder.Command, 80), Unit: sanitize(holder.Unit, 120)}
}

func aptLockBusy(output []byte) bool {
	value := strings.ToLower(string(output))
	for _, marker := range []string{"lock file is already taken", "could not get lock", "unable to acquire", "unable to lock", "failed to lock", "is another process using it"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func renderAutoConfig(policy entity.Policy) []byte {
	updateDays, upgradeDays := policy.UpdatePackageListsDays, policy.UnattendedUpgradeDays
	if !policy.Enabled || policy.Mode == entity.ModeMonitorOnly {
		upgradeDays = 0
	}
	return []byte(fmt.Sprintf("// Managed by Beszel Plus.\nAPT::Periodic::Update-Package-Lists \"%d\";\nAPT::Periodic::Unattended-Upgrade \"%d\";\n", updateDays, upgradeDays))
}

func mergeAutoConfig(existing []byte, policy entity.Policy) []byte {
	var preserved strings.Builder
	for line := range strings.Lines(string(existing)) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "APT::Periodic::Update-Package-Lists") || strings.HasPrefix(trimmed, "APT::Periodic::Unattended-Upgrade") || strings.Contains(trimmed, "Managed by Beszel Plus") {
			continue
		}
		preserved.WriteString(line)
	}
	if preserved.Len() > 0 && !strings.HasSuffix(preserved.String(), "\n") {
		preserved.WriteByte('\n')
	}
	preserved.Write(renderAutoConfig(policy))
	return []byte(preserved.String())
}

func (h *Helper) renderUnattendedConfig(policy entity.Policy) ([]byte, error) {
	patterns := []string{}
	switch policy.Mode {
	case entity.ModeSecurity:
		patterns = []string{
			// Keep both Debian forms for current *-security codenames and older
			// security archives whose label changed without changing codename.
			"origin=Debian,codename=${distro_codename}-security,label=Debian-Security",
			"origin=Debian,codename=${distro_codename},label=Debian-Security",
			"origin=Ubuntu,codename=${distro_codename}-security",
			"origin=Raspbian,codename=${distro_codename}-security,label=Raspbian-Security",
			"origin=Raspbian,codename=${distro_codename},label=Raspbian-Security",
		}
	case entity.ModeOfficialAll:
		patterns = []string{"origin=Debian,codename=${distro_codename}", "origin=Ubuntu,codename=${distro_codename}", "origin=Raspbian,codename=${distro_codename}", "origin=Raspberry Pi Foundation,codename=${distro_codename}"}
	case entity.ModeCustom:
		repos, err := h.detectRepositories()
		if err != nil {
			return nil, err
		}
		byID := map[string]entity.Repository{}
		for _, repo := range repos {
			byID[repo.ID] = repo
		}
		for _, id := range policy.AllowedRepositories {
			repo, ok := byID[id]
			if !ok {
				return nil, errors.New("repository not found")
			}
			patterns = append(patterns, fmt.Sprintf("origin=%s,codename=%s,label=%s", repo.Origin, repo.Codename, repo.Label))
		}
	}
	var b strings.Builder
	b.WriteString("// Managed by Beszel Plus. Do not edit.\nUnattended-Upgrade::Origins-Pattern {\n")
	for _, pattern := range patterns {
		if !safePattern(pattern) {
			return nil, errors.New("unsafe repository metadata")
		}
		fmt.Fprintf(&b, "  \"%s\";\n", pattern)
	}
	fmt.Fprintf(&b, "};\nUnattended-Upgrade::Automatic-Reboot \"%t\";\nUnattended-Upgrade::Automatic-Reboot-Time \"%s\";\nUnattended-Upgrade::Remove-Unused-Dependencies \"%t\";\n", policy.AutomaticReboot, policy.AutomaticRebootTime, policy.RemoveUnusedDependencies)
	return []byte(b.String()), nil
}

func safePattern(value string) bool {
	return len(value) <= 512 && !strings.ContainsAny(value, "\"'\r\n\\") && !strings.Contains(value, "..")
}

func (h *Helper) detectRepositories() ([]entity.Repository, error) {
	files, err := filepath.Glob(h.path("/var/lib/apt/lists/*_InRelease"))
	if err != nil {
		return nil, err
	}
	repos := make([]entity.Repository, 0, len(files))
	for _, path := range files {
		data, err := readLimited(path, 256*1024)
		if err != nil {
			continue
		}
		repo := parseInRelease(data)
		if repo.Origin == "" || repo.Codename == "" || !safePattern(repo.Origin+repo.Label+repo.Codename+repo.Archive) {
			continue
		}
		repo.Site = siteFromListName(filepath.Base(path))
		repo.Official = isOfficial(repo)
		// Files in /var/lib/apt/lists were accepted by APT; official/vendor
		// classification remains a separate policy decision.
		repo.Trusted = true
		sum := sha256.Sum256([]byte(repo.Origin + "\x00" + repo.Label + "\x00" + repo.Codename + "\x00" + repo.Site))
		repo.ID = hex.EncodeToString(sum[:8])
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	return repos, nil
}

func parseInRelease(data []byte) entity.Repository {
	repo := entity.Repository{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Origin":
			repo.Origin = value
		case "Label":
			repo.Label = value
		case "Suite":
			repo.Archive = value
		case "Codename":
			repo.Codename = value
		case "Components":
			repo.Components = strings.Fields(value)
		case "Architectures":
			repo.Architectures = strings.Fields(value)
		}
	}
	return repo
}

func isOfficial(repo entity.Repository) bool {
	switch strings.ToLower(repo.Origin) {
	case "debian", "ubuntu", "canonical", "raspbian", "raspberry pi foundation":
		return true
	}
	return false
}
func siteFromListName(name string) string {
	if before, _, ok := strings.Cut(name, "_dists_"); ok {
		return strings.ReplaceAll(before, "_", ".")
	}
	return ""
}

func (h *Helper) stateDir() string { return h.path("/var/lib/beszel-maintenance") }
func (h *Helper) writePolicy(policy entity.Policy) error {
	data, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(h.stateDir(), "policy.json"), append(data, '\n'), 0o600)
}
func (h *Helper) readPolicy() (entity.Policy, error) {
	data, err := readLimited(filepath.Join(h.stateDir(), "policy.json"), 64*1024)
	if os.IsNotExist(err) {
		policy := entity.DefaultPolicy()
		// Existing/manual installations are represented as monitor-only until an
		// administrator explicitly validates and applies a Beszel policy.
		policy.Enabled = false
		policy.Mode = entity.ModeMonitorOnly
		return policy, nil
	}
	if err != nil {
		return entity.Policy{}, err
	}
	var policy entity.Policy
	err = json.Unmarshal(data, &policy)
	return policy, err
}
func (h *Helper) saveStatus(status entity.Response) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(h.stateDir(), "operation.json"), append(data, '\n'), 0o600)
}
func (h *Helper) readStatus() (entity.Response, error) {
	data, err := readLimited(filepath.Join(h.stateDir(), "operation.json"), 128*1024)
	if err != nil {
		return entity.Response{}, err
	}
	var response entity.Response
	err = json.Unmarshal(data, &response)
	return response, err
}
func (h *Helper) cachedResponse(key string) (entity.Response, bool) {
	if key == "" {
		return entity.Response{}, false
	}
	response, err := h.readStatus()
	return response, err == nil && response.IdempotencyKey == key && (response.Status == entity.StateCompleted || response.Status == entity.StateFailed)
}
func (h *Helper) complete(req entity.Request, result *entity.Result, changed bool) entity.Response {
	now := h.Now()
	if result != nil {
		result.Changed = result.Changed || changed
	}
	return entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateCompleted, Progress: 100, Result: result, IdempotencyKey: req.IdempotencyKey, FinishedAt: &now}
}
func (h *Helper) lock() (*os.File, error) {
	path := h.path("/run/beszel-maintenance.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".beszel-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}
func (h *Helper) backupFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := filepath.Join(h.stateDir(), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Base(path) + "." + h.Now().UTC().Format("20060102T150405Z") + ".bak"
	if err := atomicWrite(filepath.Join(dir, name), data, 0o600); err != nil {
		return err
	}
	return retainBackups(dir, filepath.Base(path)+".*.bak", 5)
}

func (h *Helper) migrateLegacyBackups() error {
	dir := filepath.Join(h.stateDir(), "backups")
	for _, source := range []string{h.path("/etc/apt/apt.conf.d/20auto-upgrades.beszel-backup"), h.path("/etc/apt/apt.conf.d/52beszel-plus-unattended-upgrades.beszel-backup")} {
		data, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
		name := filepath.Base(strings.TrimSuffix(source, ".beszel-backup")) + ".legacy." + h.Now().UTC().Format("20060102T150405Z") + ".bak"
		if err := atomicWrite(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
		if err := os.Remove(source); err != nil {
			return err
		}
	}
	return nil
}

func retainBackups(dir, pattern string, keep int) error {
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil {
			return err
		}
		files = files[1:]
	}
	return nil
}
func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}
func sanitize(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 && r != '\t' && r != '\n' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) > max {
		value = value[len(value)-max:]
	}
	return value
}
