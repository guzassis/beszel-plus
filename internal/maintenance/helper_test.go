package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
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
	for _, dir := range []string{"etc/apt/apt.conf.d", "etc", "var/lib/apt/lists", "var/lib/beszel-maintenance", "run"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("ID=debian\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Helper{Root: root, Runner: &fakeRunner{}, Now: func() time.Time { return time.Unix(100, 0).UTC() }}
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
	if data, _ := os.ReadFile(autoPath + ".beszel-backup"); !bytes.Equal(data, existing) {
		t.Fatalf("backup = %q", data)
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
