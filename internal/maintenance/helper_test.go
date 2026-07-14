package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
	"golang.org/x/sys/unix"
)

type fakeRunner struct {
	calls  [][]string
	output []byte
	err    error
}

type helperRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (fn helperRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return fn(ctx, name, args...)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.output, r.err
}

func testHelper(t *testing.T) *Helper {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"etc/apt/apt.conf.d", "etc", "proc", "var/lib/apt/lists", "var/lib/beszel-maintenance", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Helper{Root: root, Runner: &fakeRunner{}, Now: func() time.Time { return time.Unix(100, 0).UTC() }}
}

func TestAPTActivityRequiresARealKnownLock(t *testing.T) {
	h := testHelper(t)
	if err := os.MkdirAll(h.path("/proc/4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("/proc/4242/comm"), []byte("apt-get\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("/proc/4242/cgroup"), []byte("0::/system.slice/apt-daily.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lock := h.aptActivity(); lock != nil {
		t.Fatalf("process name without a lock was treated as busy: %#v", lock)
	}

	for _, lockPath := range []string{"/var/lib/dpkg/lock-frontend", "/var/lib/apt/lists/lock"} {
		t.Run(filepath.Base(lockPath), func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(h.path(lockPath)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.path(lockPath), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(h.path(lockPath))
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			line := fmt.Sprintf("1: POSIX ADVISORY WRITE 4242 %x:%x:%d 0 EOF\n", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
			if err := os.WriteFile(h.path("/proc/locks"), []byte(line), 0o644); err != nil {
				t.Fatal(err)
			}
			lock := h.aptActivity()
			if lock == nil || lock.File != lockPath || lock.PID != 4242 || lock.Command != "apt-get" || lock.Unit != "apt-daily.service" {
				t.Fatalf("real lock was not attributed: %#v", lock)
			}
			if err := os.Remove(h.path(lockPath)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAPTLockMessagesAreNarrowlyClassified(t *testing.T) {
	for _, output := range []string{"Could not get lock /var/lib/dpkg/lock-frontend", "Unable to lock directory /var/lib/apt/lists", "Lock file is already taken, exiting"} {
		if !aptLockBusy([]byte(output)) {
			t.Fatalf("lock output not recognized: %q", output)
		}
	}
	for _, output := range []string{"apt-get --simulate", "apt-get -o Debug::NoLocking=true upgrade", "permission denied", "invalid configuration"} {
		if aptLockBusy([]byte(output)) {
			t.Fatalf("non-lock output treated as busy: %q", output)
		}
	}
}

func TestSchedulePoweroffUsesSystemdTransientTimer(t *testing.T) {
	h := testHelper(t)
	runner := h.Runner.(*fakeRunner)
	result, changed, err := h.schedulePoweroff(context.Background(), 900)
	if err != nil || !changed || result.PoweroffStatus == nil || !result.PoweroffStatus.Scheduled {
		t.Fatalf("result=%#v changed=%v err=%v", result, changed, err)
	}
	var flattened []string
	for _, call := range runner.calls {
		flattened = append(flattened, strings.Join(call, " "))
	}
	joined := strings.Join(flattened, "\n")
	if !strings.Contains(joined, "systemd-run --unit=beszel-poweroff --collect --on-active=900s /usr/bin/systemctl poweroff") {
		t.Fatalf("unsafe poweroff invocation: %s", joined)
	}
}

func TestPoweroffDelayIsBounded(t *testing.T) {
	req := entity.Request{Version: entity.ProtocolVersion, RequestID: "power-request", Operation: entity.SchedulePoweroff, IdempotencyKey: "power-request", DelaySeconds: 604801}
	if err := entity.ValidateRequest(req); err == nil {
		t.Fatal("expected excessive delay to be rejected")
	}
}

func TestPendingPolicyUsesBoundedMetadataSchema(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	response := entity.Response{ErrorCode: "apt_lock_busy", Result: &entity.Result{Policy: &policy}}
	if err := h.savePendingPolicy(response); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(h.stateDir(), "pending-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"policy", "created_at", "last_attempt_at", "attempt_count", "last_error_code", "next_attempt_at"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing %s in %s", field, data)
		}
	}
	pending, err := h.readPendingPolicyData()
	if err != nil {
		t.Fatal(err)
	}
	if pending.AttemptCount != 1 || pending.NextAttemptAt.Sub(pending.LastAttemptAt) != time.Minute {
		t.Fatalf("unexpected pending metadata: %#v", pending)
	}
}

func TestDryRunFailuresAreStructured(t *testing.T) {
	for _, test := range []struct {
		output string
		err    error
		code   string
	}{
		{"permission denied", errors.New("exit 1"), "permission_denied"},
		{"syntax error in configuration", errors.New("exit 1"), "invalid_configuration"},
		{"unattended-upgrade: not found", exec.ErrNotFound, "command_missing"},
		{"unexpected permanent failure", errors.New("exit 1"), "apt_dry_run_failed"},
	} {
		failure := classifyDryRunFailure([]byte(test.output), test.err)
		if failure.code != test.code || failure.retryable {
			t.Fatalf("output %q classified as %#v", test.output, failure)
		}
	}
}

func TestRealAPTLockReleaseAndTimeout(t *testing.T) {
	prepare := func(t *testing.T, h *Helper) {
		t.Helper()
		lockPath := h.path("/var/lib/dpkg/lock-frontend")
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(lockPath)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		line := fmt.Sprintf("1: POSIX ADVISORY WRITE 4242 %x:%x:%d 0 EOF\n", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
		if err := os.WriteFile(h.path("/proc/locks"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h := testHelper(t)
	prepare(t, h)
	h.APTLockTimeout = 100 * time.Millisecond
	h.APTRetryBase = time.Millisecond
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = os.WriteFile(h.path("/proc/locks"), nil, 0o644)
	}()
	if err := h.waitForAPT(context.Background()); err != nil {
		t.Fatalf("released lock did not unblock retry: %v", err)
	}

	h = testHelper(t)
	prepare(t, h)
	h.APTLockTimeout = time.Millisecond
	h.APTRetryBase = 2 * time.Millisecond
	err := h.waitForAPT(context.Background())
	var typed *operationError
	if !errors.As(err, &typed) || typed.code != "apt_lock_busy" || typed.lock == nil || typed.lock.File != "/var/lib/dpkg/lock-frontend" {
		t.Fatalf("held lock did not return structured timeout: %#v", err)
	}
}

func req(op entity.Operation) entity.Request {
	return entity.Request{Version: entity.ProtocolVersion, RequestID: "request-123", Operation: op, IdempotencyKey: "idem-123"}
}

func TestApplyPolicyWritesOnlyManagedFilesAndBackup(t *testing.T) {
	h := testHelper(t)
	existing := []byte("// custom setting\nAPT::Periodic::Download-Upgradeable-Packages \"1\";\nAPT::Periodic::Unattended-Upgrade \"0\";\n")
	autoPath := h.path("/etc/apt/apt.conf.d/20auto-upgrades")
	if err := os.WriteFile(autoPath, existing, 0o644); err != nil {
		t.Fatal(err)
	}
	policy := entity.DefaultPolicy()
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &policy
	response := h.Execute(context.Background(), request)
	if response.Status != entity.StateCompleted {
		t.Fatalf("response: %#v", response)
	}
	backups, err := filepath.Glob(filepath.Join(h.stateDir(), "backups", "20auto-upgrades.*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup files = %v, %v", backups, err)
	}
	if data, _ := os.ReadFile(backups[0]); !bytes.Equal(data, existing) {
		t.Fatalf("backup = %q", data)
	}
	if _, err := os.Stat(autoPath + ".beszel-backup"); !os.IsNotExist(err) {
		t.Fatal("legacy backup was left inside apt.conf.d")
	}
	if info, err := os.Stat(filepath.Dir(backups[0])); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("backup directory permissions = %v, %v", info, err)
	}
	if _, err := os.Stat(h.path("/etc/apt/apt.conf.d/52beszel-plus-unattended-upgrades")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(autoPath); !bytes.Contains(data, []byte("Download-Upgradeable-Packages")) || bytes.Contains(data, []byte("Unattended-Upgrade \"0\"")) {
		t.Fatalf("existing config was not merged safely: %s", data)
	}
	if _, err := os.Stat(h.path("/etc/apt/apt.conf.d/50unattended-upgrades")); !os.IsNotExist(err) {
		t.Fatal("helper touched vendor configuration")
	}
}

func TestUnattendedDryRunRetriesTransientAPTLock(t *testing.T) {
	attempts := 0
	h := testHelper(t)
	h.APTLockTimeout = time.Second
	h.APTRetryBase = time.Millisecond
	h.Runner = helperRunnerFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "unattended-upgrade" {
			return nil, errors.New("inactive")
		}
		attempts++
		if attempts == 1 {
			return []byte("Lock file is already taken, exiting"), errors.New("exit 1")
		}
		return []byte("dry-run complete"), nil
	})
	output, err := h.unattendedDryRun(context.Background())
	if err != nil || attempts != 2 || !bytes.Contains(output, []byte("complete")) {
		t.Fatalf("output=%q attempts=%d err=%v", output, attempts, err)
	}
}

func TestUnattendedDryRunReturnsTypedBusyErrorAtTimeout(t *testing.T) {
	h := testHelper(t)
	h.APTLockTimeout = time.Millisecond
	h.APTRetryBase = 2 * time.Millisecond
	h.Runner = helperRunnerFunc(func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name != "unattended-upgrade" {
			return nil, errors.New("inactive")
		}
		return []byte("Could not get lock /var/lib/dpkg/lock-frontend"), errors.New("exit 100")
	})
	_, err := h.unattendedDryRun(context.Background())
	var typed *operationError
	if !errors.As(err, &typed) || typed.code != "apt_lock_busy" || !typed.retryable || typed.stage != "unattended_upgrade_dry_run" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestRetryablePolicyPersistsUntilSuccessfulRetry(t *testing.T) {
	h := testHelper(t)
	h.RunSystemCommands = true
	h.APTLockTimeout = time.Millisecond
	h.APTRetryBase = 2 * time.Millisecond
	h.Runner = helperRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "is-enabled" {
			return []byte("disabled\n"), nil
		}
		if name == "unattended-upgrade" {
			return []byte("Could not get lock /var/lib/dpkg/lock-frontend"), errors.New("exit 1")
		}
		return nil, nil
	})
	policy := entity.DefaultPolicy()
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &policy
	failed := h.Execute(context.Background(), request)
	if failed.Status != entity.StateFailed || !failed.Retryable {
		t.Fatalf("expected retryable policy result: %#v", failed)
	}
	statusRequest := req(entity.GetOperationStatus)
	statusRequest.IdempotencyKey = ""
	status := h.Execute(context.Background(), statusRequest)
	if status.Result == nil || status.Result.Policy == nil || !status.Retryable {
		t.Fatalf("pending policy was not persisted: %#v", status)
	}

	h.Runner = helperRunnerFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte("dry-run complete"), nil })
	request.RequestID = "request-retry"
	request.IdempotencyKey = "idem-retry"
	completed := h.Execute(context.Background(), request)
	if completed.Status != entity.StateCompleted {
		t.Fatalf("policy retry failed: %#v", completed)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir(), "pending-policy.json")); !os.IsNotExist(err) {
		t.Fatalf("successful retry did not clear pending state: %v", err)
	}
}

func TestMigratesOnlyKnownLegacyAPTBackups(t *testing.T) {
	h := testHelper(t)
	known := h.path("/etc/apt/apt.conf.d/20auto-upgrades.beszel-backup")
	unknown := h.path("/etc/apt/apt.conf.d/vendor.beszel-backup")
	if err := os.WriteFile(known, []byte("known"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.migrateLegacyBackups(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(known); !os.IsNotExist(err) {
		t.Fatal("known legacy backup was not moved")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unrelated backup was modified")
	}
	moved, _ := filepath.Glob(filepath.Join(h.stateDir(), "backups", "20auto-upgrades.legacy.*.bak"))
	if len(moved) != 1 {
		t.Fatalf("migrated backups: %v", moved)
	}
}

func TestOfficialAllIncludesRaspberryPiAndUsesCodename(t *testing.T) {
	h := testHelper(t)
	policy := entity.DefaultPolicy()
	policy.Mode = entity.ModeOfficialAll
	data, err := h.renderUnattendedConfig(policy)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "Raspberry Pi Foundation") || !strings.Contains(text, "${distro_codename}") {
		t.Fatalf("config: %s", text)
	}
	if strings.Contains(text, "stable") || strings.Contains(text, "oldstable") {
		t.Fatal("policy must not bind to moving archive names")
	}
}

func TestSafePolicyModesAndRebootDefaults(t *testing.T) {
	h := testHelper(t)
	security := entity.DefaultPolicy()
	if security.AutomaticReboot {
		t.Fatal("automatic reboot must default to false")
	}
	data, err := h.renderUnattendedConfig(security)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "origin=Debian,codename=${distro_codename}-security,label=Debian-Security") ||
		!strings.Contains(text, "origin=Debian,codename=${distro_codename},label=Debian-Security") ||
		!strings.Contains(text, "origin=Ubuntu,codename=${distro_codename}-security") || strings.Contains(text, "Tailscale") {
		t.Fatalf("unsafe security policy: %s", text)
	}
	monitor := security
	monitor.Mode = entity.ModeMonitorOnly
	if config := string(renderAutoConfig(monitor)); !strings.Contains(config, `APT::Periodic::Unattended-Upgrade "0"`) {
		t.Fatalf("monitor-only mode enabled upgrades: %s", config)
	}
}

func TestCapabilitiesRejectUnsupportedPlatform(t *testing.T) {
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=fedora\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caps := h.capabilities()
	if caps.UpdateManagement || caps.PolicyWrite || caps.RunUpgrade || caps.SupportedPlatform != "fedora" {
		t.Fatalf("unsupported platform advertised management: %#v", caps)
	}
	if response := h.Execute(context.Background(), req(entity.RunUnattendedUpgrades)); response.Status != entity.StateFailed || !strings.Contains(response.Error, "unsupported") {
		t.Fatalf("unsupported platform executed an APT operation: %#v", response)
	}
}

func TestRepositoryClassificationAndThirdPartyConfirmation(t *testing.T) {
	h := testHelper(t)
	write := func(name, origin, label string) {
		content := "Origin: " + origin + "\nLabel: " + label + "\nSuite: oldstable\nCodename: bookworm\nComponents: main\nArchitectures: amd64\n\n"
		if err := os.WriteFile(h.path("/var/lib/apt/lists/"+name+"_dists_bookworm_InRelease"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("deb_debian_org_debian", "Debian", "Debian")
	write("pkgs_tailscale_com_stable_debian", "Tailscale", "Tailscale")
	repos, err := h.detectRepositories()
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos=%#v err=%v", repos, err)
	}
	var third entity.Repository
	for _, repo := range repos {
		if repo.Origin == "Tailscale" {
			third = repo
			if repo.Official {
				t.Fatal("Tailscale classified as official")
			}
		}
	}
	policy := entity.DefaultPolicy()
	policy.Mode = entity.ModeCustom
	policy.AllowedRepositories = []string{third.ID}
	if h.validatePolicy(policy) == nil {
		t.Fatal("third party accepted without confirmation")
	}
	policy.ConfirmThirdParty = true
	if err := h.validatePolicy(policy); err != nil {
		t.Fatal(err)
	}
}

func TestIPCRejectsUnknownFieldsAndReplayIsIdempotent(t *testing.T) {
	h := testHelper(t)
	var out bytes.Buffer
	if err := h.Serve(context.Background(), strings.NewReader(`{"version":1,"request_id":"request-123","operation":"get-capabilities","evil":true}`), &out); err != nil {
		t.Fatal(err)
	}
	var response entity.Response
	if json.Unmarshal(out.Bytes(), &response) != nil || response.Status != entity.StateFailed {
		t.Fatalf("response=%s", out.String())
	}
	request := req(entity.GetUpdatePolicy)
	first := h.Execute(context.Background(), request)
	second := h.Execute(context.Background(), request)
	if first.Status != second.Status || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatal("idempotent replay changed response")
	}
	conflict := request
	conflict.Operation = entity.DetectRepositories
	if response := h.Execute(context.Background(), conflict); response.Status != entity.StateFailed || !strings.Contains(response.Error, "reused") {
		t.Fatalf("replay conflict accepted: %#v", response)
	}
}

func TestIPCRejectsExcessivePayload(t *testing.T) {
	h := testHelper(t)
	var out bytes.Buffer
	payload := strings.Repeat("x", maxIPCRequestBytes+1)
	if err := h.Serve(context.Background(), strings.NewReader(payload), &out); err != nil {
		t.Fatal(err)
	}
	var response entity.Response
	if json.Unmarshal(out.Bytes(), &response) != nil || response.Status != entity.StateFailed || !strings.Contains(response.Error, "large") {
		t.Fatalf("response=%s", out.String())
	}
}

func TestConcurrentOperationIsRejected(t *testing.T) {
	h := testHelper(t)
	lock, err := h.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	policy := entity.DefaultPolicy()
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &policy
	response := h.Execute(context.Background(), request)
	if response.Status != entity.StateFailed || !strings.Contains(response.Error, "another") {
		t.Fatalf("response=%#v", response)
	}
}

func TestApplyPolicyRollsBackWhenAPTValidationFails(t *testing.T) {
	h := testHelper(t)
	runner := &fakeRunner{err: context.DeadlineExceeded, output: []byte("validation failed")}
	h.Runner, h.RunSystemCommands = runner, true
	autoPath := h.path("/etc/apt/apt.conf.d/20auto-upgrades")
	original := []byte("original\n")
	if err := os.WriteFile(autoPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	policy := entity.DefaultPolicy()
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &policy
	request.IdempotencyKey = "rollback-test"
	response := h.Execute(context.Background(), request)
	if response.Status != entity.StateFailed {
		t.Fatalf("response=%#v", response)
	}
	data, err := os.ReadFile(autoPath)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("rollback=%q err=%v", data, err)
	}
}

func TestApplyPolicyRestoresTimerStateOnRollback(t *testing.T) {
	h := testHelper(t)
	h.RunSystemCommands = true
	var calls []string
	h.Runner = helperRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if name == "systemctl" && len(args) > 0 && args[0] == "is-enabled" {
			return []byte("enabled\n"), nil
		}
		if name == "apt-config" {
			return []byte("invalid configuration"), errors.New("invalid")
		}
		return nil, nil
	})
	policy := entity.DefaultPolicy()
	request := req(entity.ApplyUpdatePolicy)
	request.Policy = &policy
	request.IdempotencyKey = "timer-rollback"
	if response := h.Execute(context.Background(), request); response.Status != entity.StateFailed {
		t.Fatalf("response=%#v", response)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "systemctl enable --now apt-daily.timer") || !strings.Contains(joined, "systemctl enable --now apt-daily-upgrade.timer") {
		t.Fatalf("timer state was not restored:\n%s", joined)
	}
}

func TestOperationStatusSurvivesHelperRestart(t *testing.T) {
	h := testHelper(t)
	request := req(entity.GetUpdatePolicy)
	request.IdempotencyKey = "persist-test"
	want := h.Execute(context.Background(), request)
	restarted := &Helper{Root: h.Root, Runner: &fakeRunner{}, Now: h.Now}
	statusReq := req(entity.GetOperationStatus)
	statusReq.IdempotencyKey = "status-query"
	got := restarted.Execute(context.Background(), statusReq)
	if got.RequestID != want.RequestID || got.Status != want.Status {
		t.Fatalf("status not recovered: got=%#v want=%#v", got, want)
	}
}
