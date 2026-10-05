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
	"sync"
	"syscall"
	"time"

	"github.com/henrygd/beszel/internal/aptstatus"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	powerentity "github.com/henrygd/beszel/internal/entities/power"
	powerexec "github.com/henrygd/beszel/internal/power"
)

const maxIPCRequestBytes = 64 * 1024
const maxCommandOutputBytes = 64 * 1024
const maxStructuredCommandOutputBytes = 16 * 1024 * 1024
const maxAPTCommandOutputBytes = 2 * 1024 * 1024

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type limitedRunner interface {
	RunLimited(context.Context, int, string, ...string) ([]byte, error)
}

type limitedSupervisedRunner interface {
	RunSupervisedLimited(context.Context, int, string, func(int), func(), ...string) ([]byte, error, bool)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return (ExecRunner{}).RunLimited(ctx, maxCommandOutputBytes, name, args...)
}

func (ExecRunner) RunLimited(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Privileged commands must not inherit a caller's deleted or untrusted CWD.
	cmd.Dir = "/"
	cmd.Env = commandEnvironment()
	if limit < 1 {
		limit = maxCommandOutputBytes
	}
	output := &limitedOutputBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	if outputErr := output.outputError(name); outputErr != nil {
		return output.Bytes(), outputErr
	}
	return output.Bytes(), err
}

func (ExecRunner) RunSupervised(ctx context.Context, name string, onStart func(int), onTimeout func(), args ...string) ([]byte, error, bool) {
	return (ExecRunner{}).RunSupervisedLimited(ctx, maxCommandOutputBytes, name, onStart, onTimeout, args...)
}

func (ExecRunner) RunSupervisedLimited(ctx context.Context, limit int, name string, onStart func(int), onTimeout func(), args ...string) ([]byte, error, bool) {
	if err := ctx.Err(); err != nil {
		return nil, err, false
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = "/"
	cmd.Env = commandEnvironment()
	if limit < 1 {
		limit = maxCommandOutputBytes
	}
	output := &limitedOutputBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return nil, err, false
	}
	if onStart != nil {
		onStart(cmd.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if outputErr := output.outputError(name); outputErr != nil {
			err = outputErr
		}
		return output.Bytes(), err, false
	case <-ctx.Done():
		if onTimeout != nil {
			onTimeout()
		}
		// Keep the helper's APT lock and systemd service alive until the child
		// exits. The observed deadline is recorded, but the child is never killed.
		err := <-done
		if outputErr := output.outputError(name); outputErr != nil {
			err = outputErr
		}
		return output.Bytes(), err, true
	}
}

func commandEnvironment() []string {
	env := os.Environ()
	filtered := env[:0]
	for _, value := range env {
		if !strings.HasPrefix(value, "LC_ALL=") && !strings.HasPrefix(value, "LANG=") {
			filtered = append(filtered, value)
		}
	}
	return append(filtered, "LC_ALL=C", "LANG=C")
}

type limitedOutputBuffer struct {
	mu       sync.Mutex
	data     []byte
	limit    int
	exceeded bool
}

func (b *limitedOutputBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.limit - len(b.data)
	if available > 0 {
		b.data = append(b.data, value[:min(available, len(value))]...)
	}
	if len(value) > available {
		b.exceeded = true
	}
	return len(value), nil
}

func (b *limitedOutputBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...)
}

func (b *limitedOutputBuffer) outputError(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.exceeded {
		return nil
	}
	return fmt.Errorf("%s output exceeded %d byte limit", name, b.limit)
}

type Helper struct {
	Root              string
	Runner            Runner
	EthtoolPath       string
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
	// WOL diagnostics are a read-only probe. Keep them independent from the
	// update-state migration so a stale backup cannot prevent the Agent from
	// learning the host's actual Wake-on-LAN capability.
	if req.Operation == entity.ProbeWOL {
		result, err := h.probeWOL(ctx, req.Interfaces)
		if err != nil {
			return failureFor(req, err)
		}
		return h.complete(req, result, false)
	}
	if err := h.migrateLegacyBackups(); err != nil {
		return failureFor(req, &operationError{code: "filesystem_write_failed", stage: "legacy_backup_migration", message: err.Error()})
	}
	if req.Operation != entity.RunUnattendedUpgrades {
		if cached, ok := h.cachedResponse(req.IdempotencyKey); ok {
			if cached.Operation != req.Operation {
				return failed(req, "idempotency key reused for another operation")
			}
			return cached
		}
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
		pendingPath := filepath.Join(h.stateDir(), "pending-policy.json")
		if _, statErr := os.Stat(pendingPath); statErr == nil {
			if pending, err := h.readPendingPolicy(); err == nil {
				return pending
			} else {
				return failureFor(req, &operationError{code: "pending_policy_superseded", stage: "pending_policy_validation", message: err.Error()})
			}
		} else if !os.IsNotExist(statErr) {
			return failureFor(req, &operationError{code: "pending_policy_unavailable", stage: "pending_policy_read", message: statErr.Error()})
		}
		if status, err := h.readStatus(); err == nil {
			return status
		}
		return failed(req, "operation status unavailable")
	}
	if req.Operation == entity.GetUpdateCycleStatus {
		return h.updateCycleStatus(req)
	}
	if caps := h.capabilities(); !caps.UpdateManagement {
		return failed(req, "unsupported platform")
	}
	lock, err := h.lock()
	if err != nil {
		return failed(req, "another maintenance operation is running")
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }()
	if req.Operation == entity.RunUnattendedUpgrades {
		response := h.executeUpdateCycle(ctx, req)
		_ = h.saveStatus(response)
		return response
	}
	start := h.Now()
	running := entity.Response{Version: entity.ProtocolVersion, RequestID: req.RequestID, Operation: req.Operation, Status: entity.StateRunning, Progress: 5, IdempotencyKey: req.IdempotencyKey, StartedAt: &start}
	_ = h.saveStatus(running)
	result, changed, runErr := h.run(ctx, req)
	var response entity.Response
	if runErr != nil {
		response = failureFor(req, runErr)
		response.StartedAt = &start
		if req.Operation == entity.ApplyUpdatePolicy && response.Retryable {
			response.Result = &entity.Result{Policy: req.Policy, PolicyRevision: policyRevision(*req.Policy), ExpectedPolicyRevision: req.ExpectedPolicyRevision}
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
	Policy             entity.Policy `json:"policy"`
	PolicyRevision     string        `json:"policy_revision"`
	BasePolicyRevision string        `json:"base_policy_revision"`
	CreatedAt          time.Time     `json:"created_at"`
	LastAttemptAt      time.Time     `json:"last_attempt_at"`
	AttemptCount       uint32        `json:"attempt_count"`
	LastErrorCode      string        `json:"last_error_code"`
	NextAttemptAt      time.Time     `json:"next_attempt_at"`
}

var pendingBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour}

func (h *Helper) savePendingPolicy(response entity.Response) error {
	if response.Result == nil || response.Result.Policy == nil {
		return errors.New("pending policy is missing")
	}
	now := h.Now()
	current, err := h.readPolicy()
	if err != nil {
		return err
	}
	pending := pendingPolicy{Policy: *response.Result.Policy, PolicyRevision: policyRevision(*response.Result.Policy), BasePolicyRevision: policyRevision(current), CreatedAt: now, LastAttemptAt: now, AttemptCount: 1, LastErrorCode: response.ErrorCode}
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
	current, err := h.readPolicy()
	if err != nil {
		return entity.Response{}, err
	}
	baseRevision := pending.BasePolicyRevision
	targetRevision := pending.PolicyRevision
	if targetRevision == "" {
		targetRevision = policyRevision(pending.Policy)
	}
	if baseRevision == "" {
		if policyRevision(current) != targetRevision {
			return entity.Response{}, errors.New("legacy pending policy cannot be resumed after the current policy changed")
		}
		baseRevision = policyRevision(current)
	}
	if policyRevision(current) != baseRevision {
		return entity.Response{}, errors.New("pending policy was superseded by a newer policy revision")
	}
	return entity.Response{Version: entity.ProtocolVersion, RequestID: "pending-policy", Operation: entity.ApplyUpdatePolicy, Status: entity.StateFailed, Error: "policy application is pending", ErrorCode: pending.LastErrorCode, Retryable: true, Result: &entity.Result{Policy: &pending.Policy, PolicyRevision: targetRevision, ExpectedPolicyRevision: baseRevision}, FinishedAt: &pending.LastAttemptAt, NextAttemptAt: &pending.NextAttemptAt}, nil
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
		return &entity.Result{Policy: &policy, PolicyRevision: policyRevision(policy)}, false, err
	case entity.DetectRepositories:
		repos, err := h.detectRepositories()
		return &entity.Result{Repositories: repos}, false, err
	case entity.ValidateUpdatePolicy:
		if err := h.validatePolicy(*req.Policy); err != nil {
			return nil, false, err
		}
		return &entity.Result{Policy: req.Policy, Output: "policy valid"}, false, nil
	case entity.ApplyUpdatePolicy:
		if req.ExpectedPolicyRevision != "" {
			current, err := h.readPolicy()
			if err != nil {
				return nil, false, err
			}
			if policyRevision(current) != req.ExpectedPolicyRevision {
				return nil, false, &operationError{code: "policy_revision_conflict", stage: "policy_validation", message: "pending policy was superseded by a newer policy revision"}
			}
		}
		return h.applyPolicy(ctx, req)
	case entity.AdoptUpdatePolicy:
		return h.adoptLegacyPolicy(ctx, req)
	case entity.InstallUpdateDependencies:
		return h.installDependencies(ctx)
	case entity.RunUpdateDryRun:
		out, err := h.unattendedDryRun(ctx)
		return &entity.Result{Output: sanitize(string(out), maxCommandOutputBytes)}, false, err
	case entity.RunUnattendedUpgrades:
		return nil, false, errors.New("cycle execution must be dispatched under the maintenance lock")
	case entity.GetUpdateCycleStatus:
		return nil, false, errors.New("cycle status queries are read through Execute")
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
	return &entity.Capabilities{UpdateMonitoring: true, UpdateManagement: supported, PrivilegedHelper: true, PolicyRead: supported, PolicyWrite: supported, DryRun: supported, RunUpgrade: supported, AutomaticReboot: supported, SupportedPlatform: platform, HelperVersion: HelperVersion(), ProtocolVersion: entity.ProtocolVersion, MinAgentVersion: MinAgentVersion, MaxProtocolVersion: entity.ProtocolVersion, BuildCommit: BuildCommit, Power: h.powerCapabilities(), UpdateCycle: supported}
}

func (h *Helper) powerCapabilities() *powerentity.Capabilities {
	return &powerentity.Capabilities{PowerManagement: true, Shutdown: true, CancelShutdown: true, MaxDelaySeconds: 604800}
}

func (h *Helper) probeWOL(ctx context.Context, names []string) (*entity.Result, error) {
	path := h.ethtoolPath()
	items := make([]powerentity.InterfaceDiagnostic, 0, len(names))
	for _, name := range names {
		item := powerentity.InterfaceDiagnostic{Interface: name, Type: "ethernet", Physical: true, WOLProbePath: path}
		if !h.physicalEthernetInterface(name) {
			item.Physical = false
			item.WOLProbeError = "interface is not a physical Ethernet interface"
			items = append(items, item)
			continue
		}
		output, commandErr := h.Runner.Run(ctx, path, name)
		supported, enabled, parseErr := powerexec.ParseEthtoolWOL(output)
		if parseErr != nil {
			message := sanitize(parseErr.Error(), 256)
			if commandErr != nil {
				detail := sanitize(string(output), 256)
				if detail == "" {
					detail = sanitize(commandErr.Error(), 256)
				}
				message = "ethtool probe failed: " + detail
			}
			item.WOLProbeError = message
		} else {
			item.WOLSupported = supported
			item.WOLEnabled = enabled
		}
		items = append(items, item)
	}
	return &entity.Result{PowerInterfaces: items}, nil
}

func (h *Helper) ethtoolPath() string {
	if strings.TrimSpace(h.EthtoolPath) != "" {
		return h.EthtoolPath
	}
	for _, candidate := range []string{"/usr/sbin/ethtool", "/sbin/ethtool", "/usr/bin/ethtool"} {
		if _, err := os.Stat(h.path(candidate)); err == nil {
			return candidate
		}
	}
	return "/usr/sbin/ethtool"
}

func (h *Helper) physicalEthernetInterface(name string) bool {
	base := h.path(filepath.Join("/sys/class/net", name))
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return false
	}
	return true
}

func (h *Helper) schedulePoweroff(ctx context.Context, delay uint32) (*entity.Result, bool, error) {
	lock, err := h.aptActivity()
	if err != nil {
		return nil, false, aptLockInspectionError("power_guard", err)
	}
	if lock != nil {
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
		for _, other := range repos {
			if other.ID != repo.ID && other.Origin == repo.Origin && other.Label == repo.Label && other.Codename == repo.Codename && other.Archive == repo.Archive && other.Site == repo.Site {
				return errors.New("selected repository cannot be isolated from another repository with identical APT metadata")
			}
		}
	}
	return nil
}

func (h *Helper) applyPolicy(ctx context.Context, req entity.Request) (*entity.Result, bool, error) {
	policy := *req.Policy
	if err := h.validatePolicy(policy); err != nil {
		return nil, false, err
	}
	runSystemCommands := h.Root == "" || h.Root == "/" || h.RunSystemCommands
	if runSystemCommands {
		lock, err := h.aptActivity()
		if err != nil {
			return nil, false, aptLockInspectionError("unattended_upgrade_dry_run", err)
		}
		if lock != nil {
			return nil, false, &operationError{code: "apt_lock_busy", stage: "unattended_upgrade_dry_run", retryable: true, message: "APT is currently in use; policy validation was postponed", lock: lock}
		}
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
	return &entity.Result{Policy: &policy, PolicyRevision: policyRevision(policy), Changed: true, Output: "policy applied"}, true, nil
}

func (h *Helper) adoptLegacyPolicy(ctx context.Context, req entity.Request) (*entity.Result, bool, error) {
	policy, metadata, exists, err := h.readPolicyDocument()
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return &entity.Result{Policy: &policy, PolicyRevision: policyRevision(policy), Output: "no legacy policy to adopt"}, false, nil
	}
	if metadata.AdoptionVersion >= 1 {
		return &entity.Result{Policy: &policy, PolicyRevision: policyRevision(policy), Output: "policy already adopted"}, false, nil
	}
	if policy.Enabled && policy.Mode == entity.ModeSecurity {
		policy.Mode = entity.ModeOfficialAll
		apply := req
		apply.Operation = entity.ApplyUpdatePolicy
		apply.Policy = &policy
		result, changed, err := h.applyPolicy(ctx, apply)
		if err != nil {
			return result, changed, err
		}
		if result != nil {
			result.Output = "legacy security policy adopted as official_all"
		}
		return result, true, nil
	}
	if err := h.writePolicy(policy); err != nil {
		return nil, false, err
	}
	return &entity.Result{Policy: &policy, PolicyRevision: policyRevision(policy), Changed: true, Output: "legacy policy adopted without changing its mode"}, true, nil
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
	return h.commandWithOutputLimit(ctx, timeout, commandOutputLimit(name, args...), name, args...)
}

func (h *Helper) commandWithOutputLimit(ctx context.Context, timeout time.Duration, outputLimit int, name string, args ...string) ([]byte, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var output []byte
	var err error
	var timedOut bool
	if runner, ok := h.Runner.(limitedRunner); ok && !isAPTSubprocess(name) {
		output, err = runner.RunLimited(stepCtx, outputLimit, name, args...)
		timedOut = stepCtx.Err() != nil
	} else {
		output, err, timedOut = h.runCommandWithOutputLimit(stepCtx, outputLimit, name, args...)
	}
	if timedOut {
		return output, fmt.Errorf("%s timed out", name)
	}
	if stepCtx.Err() != nil {
		return output, fmt.Errorf("%s timed out", name)
	}
	if err != nil {
		return output, fmt.Errorf("%s failed (%s): %s", name, sanitize(err.Error(), 160), sanitize(string(output), 1024))
	}
	return output, nil
}

func (h *Helper) runCommand(ctx context.Context, name string, args ...string) ([]byte, error, bool) {
	return h.runCommandWithOutputLimit(ctx, commandOutputLimit(name, args...), name, args...)
}

func commandOutputLimit(name string, args ...string) int {
	if filepath.Base(name) == "apt-config" {
		for _, arg := range args {
			if arg == "dump" {
				return maxStructuredCommandOutputBytes
			}
		}
	}
	if isAPTSubprocess(name) {
		for _, arg := range args {
			if arg == "--dry-run" {
				return maxStructuredCommandOutputBytes
			}
		}
		return maxAPTCommandOutputBytes
	}
	return maxCommandOutputBytes
}

func (h *Helper) runCommandWithOutputLimit(ctx context.Context, outputLimit int, name string, args ...string) ([]byte, error, bool) {
	if isAPTSubprocess(name) {
		if runner, ok := h.Runner.(limitedSupervisedRunner); ok {
			return runner.RunSupervisedLimited(ctx, outputLimit, name, nil, func() { slog.Warn("APT command deadline observed; waiting for subprocess exit", "command", name) }, args...)
		}
		if runner, ok := h.Runner.(supervisedRunner); ok {
			return runner.RunSupervised(ctx, name, nil, func() { slog.Warn("APT command deadline observed; waiting for subprocess exit", "command", name) }, args...)
		}
	}
	if runner, ok := h.Runner.(limitedRunner); ok {
		output, err := runner.RunLimited(ctx, outputLimit, name, args...)
		return output, err, ctx.Err() != nil
	}
	output, err := h.Runner.Run(ctx, name, args...)
	return output, err, ctx.Err() != nil
}

func isAPTSubprocess(name string) bool {
	base := filepath.Base(name)
	return base == "apt-get" || base == "unattended-upgrade" || base == "dpkg"
}

func (h *Helper) unattendedDryRun(ctx context.Context) ([]byte, error) {
	lock, err := h.aptActivity()
	if err != nil {
		return nil, aptLockInspectionError("unattended_upgrade_dry_run", err)
	}
	if lock != nil {
		return nil, &operationError{code: "apt_lock_busy", stage: "unattended_upgrade_dry_run", retryable: true, message: "APT is currently in use; dry-run was not started", lock: lock}
	}
	timeout := h.APTLockTimeout
	if timeout <= 0 {
		timeout = 75 * time.Second
	}
	base := h.APTRetryBase
	if base <= 0 {
		base = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for attempt := 0; attempt < 2; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &operationError{code: "timeout", stage: "unattended_upgrade_dry_run", retryable: true, message: "unattended-upgrade dry-run timed out"}
		}
		stepCtx, cancel := context.WithTimeout(ctx, remaining)
		output, err, timedOut := h.runCommand(stepCtx, "unattended-upgrade", "--dry-run", "--debug")
		stepErr := stepCtx.Err()
		cancel()
		if err == nil {
			return output, nil
		}
		if stepErr != nil || timedOut {
			return output, &operationError{code: "timeout", stage: "unattended_upgrade_dry_run", retryable: true, message: "unattended-upgrade dry-run timed out"}
		}
		if !aptLockBusy(output) {
			return output, classifyDryRunFailure(output, err)
		}
		lock, inspectErr := h.aptActivity()
		if inspectErr != nil {
			return output, aptLockInspectionError("unattended_upgrade_dry_run", inspectErr)
		}
		if lock != nil {
			return output, &operationError{code: "apt_lock_busy", stage: "unattended_upgrade_dry_run", retryable: true, message: "APT became busy during policy validation", lock: lock}
		}
		if attempt == 1 {
			return output, classifyDryRunFailure(output, err)
		}
		wait := min(base, 2*time.Second)
		if !time.Now().Add(wait).Before(deadline) {
			return output, &operationError{code: "timeout", stage: "unattended_upgrade_dry_run", retryable: true, message: "unattended-upgrade dry-run timed out"}
		}
		slog.Warn("unattended-upgrade reported an unconfirmed APT lock; retrying once", "stage", "unattended_upgrade_dry_run", "retry_in", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return output, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, &operationError{code: "apt_dry_run_failed", stage: "unattended_upgrade_dry_run", message: "unattended-upgrade dry-run failed"}
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
		lock, err := h.aptActivity()
		if err != nil {
			return aptLockInspectionError("apt_lock_wait", err)
		}
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

func (h *Helper) aptActivity() (*aptLockInfo, error) {
	status, err := aptstatus.Inspect(h.Root)
	if err != nil {
		return nil, err
	}
	if !status.Busy || len(status.Holders) == 0 {
		return nil, nil
	}
	holder := status.Holders[0]
	file := ""
	if len(holder.Locks) > 0 {
		file = holder.Locks[0]
	}
	return &aptLockInfo{File: file, PID: holder.PID, Command: sanitize(holder.Command, 80), Unit: sanitize(holder.Unit, 120)}, nil
}

func aptLockInspectionError(stage string, err error) *operationError {
	return &operationError{code: "apt_lock_inspection_failed", stage: stage, retryable: true, message: "APT lock state could not be inspected: " + sanitize(err.Error(), 512)}
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
			"origin=Ubuntu,codename=${distro_codename},archive=${distro_codename}-security",
			"origin=UbuntuESMApps,codename=${distro_codename},archive=${distro_codename}-apps-security",
			"origin=UbuntuESMInfra,codename=${distro_codename},archive=${distro_codename}-infra-security",
			"origin=Raspbian,codename=${distro_codename}-security,label=Raspbian-Security",
			"origin=Raspbian,codename=${distro_codename},label=Raspbian-Security",
		}
	case entity.ModeOfficialAll:
		patterns = officialDefaultPatterns()
		repos, err := h.detectRepositories()
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			if !repo.Official || !repo.Trusted || !isCurrentReleaseOrigin(inventoryOrigin{Origin: repo.Origin, Label: repo.Label, Codename: repo.Codename, Archive: repo.Archive, Site: repositorySiteHost(repo.Site), Trusted: repo.Trusted}, h.currentCodename()) {
				continue
			}
			patterns = append(patterns, patternForRepository(repo, true))
		}
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
			patterns = append(patterns, patternForRepository(repo, true))
		}
	}
	patterns = uniqueStrings(patterns)
	var b strings.Builder
	b.WriteString("// Managed by Beszel Plus. Do not edit.\n#clear Unattended-Upgrade::Allowed-Origins;\n#clear Unattended-Upgrade::Origins-Pattern;\nUnattended-Upgrade::Allowed-Origins { };\nUnattended-Upgrade::Origins-Pattern {\n")
	for _, pattern := range patterns {
		if !safePattern(pattern) {
			return nil, errors.New("unsafe repository metadata")
		}
		fmt.Fprintf(&b, "  \"%s\";\n", pattern)
	}
	fmt.Fprintf(&b, "};\nUnattended-Upgrade::Automatic-Reboot \"%t\";\nUnattended-Upgrade::Automatic-Reboot-Time \"%s\";\nUnattended-Upgrade::Remove-Unused-Dependencies \"%t\";\n", policy.AutomaticReboot, policy.AutomaticRebootTime, policy.RemoveUnusedDependencies)
	return []byte(b.String()), nil
}

func officialDefaultPatterns() []string {
	patterns := []string{}
	for _, origin := range []string{"Debian", "Ubuntu", "Raspbian", "Raspberry Pi Foundation", "UbuntuESM", "UbuntuESMApps", "UbuntuESMInfra"} {
		for _, suffix := range []string{"", "-security", "-updates", "-backports", "-proposed", "-esm", "-esm-infra", "-esm-apps"} {
			patterns = append(patterns, fmt.Sprintf("origin=%s,codename=${distro_codename}%s", origin, suffix))
		}
	}
	return patterns
}

func patternForRepository(repo entity.Repository, includeSite bool) string {
	parts := []string{"origin=" + repo.Origin, "codename=" + repo.Codename}
	if repo.Label != "" {
		parts = append(parts, "label="+repo.Label)
	}
	if repo.Archive != "" {
		parts = append(parts, "archive="+repo.Archive)
	}
	if includeSite && repo.Site != "" {
		parts = append(parts, "site="+repositorySiteHost(repo.Site))
	}
	return strings.Join(parts, ",")
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
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
		legacySite := legacySiteFromListName(filepath.Base(path))
		sum := sha256.Sum256([]byte(repo.Origin + "\x00" + repo.Label + "\x00" + repo.Codename + "\x00" + legacySite))
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
	firstLine := true
	signedEnvelope := false
	headerComplete := true
	fieldsStarted := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if firstLine {
			signedEnvelope = line == "-----BEGIN PGP SIGNED MESSAGE-----"
			headerComplete = !signedEnvelope
			firstLine = false
		}
		if signedEnvelope && !headerComplete {
			if line == "" {
				headerComplete = true
			}
			continue
		}
		if strings.HasPrefix(line, "-----BEGIN PGP SIGNATURE-----") {
			break
		}
		if line == "" {
			if fieldsStarted {
				break
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fieldsStarted = true
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
	case "debian", "ubuntu", "canonical", "raspbian", "raspberry pi foundation", "ubuntu esm", "ubuntu esm apps", "ubuntu esm infra", "ubuntu esm infra updates", "ubuntu esm apps updates", "ubuntuesm", "ubuntuesm apps", "ubuntuesm infra", "ubuntuesminfra", "ubuntuesmapps":
		return true
	}
	return false
}
func siteFromListName(name string) string {
	if before, _, ok := strings.Cut(name, "_dists_"); ok {
		if strings.Contains(before, ".") {
			if host, _, hasPath := strings.Cut(before, "_"); hasPath {
				return host
			}
			return before
		}
		parts := strings.Split(before, "_")
		for i, part := range parts {
			switch strings.ToLower(part) {
			case "com", "org", "net", "io", "dev", "info", "biz", "uk", "de", "fr", "ca", "au", "jp":
				if i >= 2 {
					return strings.Join(parts[:i+1], ".")
				}
			}
		}
		return strings.ReplaceAll(before, "_", ".")
	}
	return ""
}

func legacySiteFromListName(name string) string {
	if before, _, ok := strings.Cut(name, "_dists_"); ok {
		return strings.ReplaceAll(before, "_", ".")
	}
	return ""
}

func (h *Helper) stateDir() string { return h.path("/var/lib/beszel-maintenance") }

type policyMetadata struct {
	AdoptionVersion uint8     `json:"adoption_version"`
	Revision        string    `json:"revision"`
	AdoptedAt       time.Time `json:"adopted_at"`
}

func (h *Helper) writePolicy(policy entity.Policy) error {
	data, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	metadata, err := json.Marshal(policyMetadata{AdoptionVersion: 1, Revision: policyRevision(policy), AdoptedAt: h.Now().UTC()})
	if err != nil {
		return err
	}
	document["_beszel"] = metadata
	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(h.stateDir(), "policy.json"), append(data, '\n'), 0o600)
}

func (h *Helper) readPolicyDocument() (entity.Policy, policyMetadata, bool, error) {
	data, err := readLimited(filepath.Join(h.stateDir(), "policy.json"), 64*1024)
	if os.IsNotExist(err) {
		policy := entity.DefaultPolicy()
		policy.Enabled = false
		policy.Mode = entity.ModeMonitorOnly
		return policy, policyMetadata{}, false, nil
	}
	if err != nil {
		return entity.Policy{}, policyMetadata{}, false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return entity.Policy{}, policyMetadata{}, true, err
	}
	var metadata policyMetadata
	if raw := document["_beszel"]; raw != nil {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return entity.Policy{}, policyMetadata{}, true, err
		}
		delete(document, "_beszel")
	}
	policyData, err := json.Marshal(document)
	if err != nil {
		return entity.Policy{}, policyMetadata{}, true, err
	}
	var policy entity.Policy
	if err := json.Unmarshal(policyData, &policy); err != nil {
		return entity.Policy{}, policyMetadata{}, true, err
	}
	return policy, metadata, true, nil
}

func (h *Helper) readPolicy() (entity.Policy, error) {
	policy, _, _, err := h.readPolicyDocument()
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
