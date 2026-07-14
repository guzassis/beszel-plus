package maintenance

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestInstallerContainsSafeMaintenanceUpgradeFlow(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{"--agent-auto-update", "--os-update-management", "--os-update-policy", "EXISTING_INSTALLATION", "beszel-maintenance.socket", "SocketMode=0660", "beszel-maintenance@.service", "systemd-analyze verify", "systemctl restart beszel-maintenance.socket", "systemctl is-active --quiet beszel-maintenance.socket", "MAINTENANCE_CONFIGURED_BEFORE", "MAINTENANCE_POLICY_EXISTED", "automatic_reboot\":false", "KEY=", "TOKEN=", "HUB_URL=", "/usr/local/libexec/beszel/maintenance-helper"} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing %q", required)
		}
	}
	for _, required := range []string{"10-beszel-connection.conf", "maintenance-smoke", "TRANSACTION_DIR", "rollback_install", "Maintenance helper protocol compatibility verified", `"retryable":true`, "maintenance_preflight", "PHASE_PREFLIGHT", "PHASE_QUIESCE", "PHASE_TRANSACTION", "PHASE_ROLLBACK", "upgrade postponed", "No binaries or configuration files were changed", "--wait-for-maintenance", "--diagnose", "upgrade-in-progress", "beszel-plus-agent-install.lock"} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing upgrade safeguard %q", required)
		}
	}
	for _, required := range []string{"--wait-for-apt", "diagnose-install --apt-required=true", "diagnose-install --apt-required=false", "DPkg::Lock::Timeout", "AGENT_ENV_PATH", `chmod 0600 "$AGENT_ENV_NEW"`, "EnvironmentFile=-$AGENT_ENV_PATH"} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing v0.2.2 safeguard %q", required)
		}
	}
	if strings.Contains(script, "Preserving existing maintenance helper") {
		t.Fatal("installer still preserves a helper from an older release")
	}
	for _, forbidden := range []string{"systemctl reboot", "shutdown -r", "reboot now"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("installer contains reboot command %q", forbidden)
		}
	}
	if !strings.Contains(script, `[ "$EXISTING_INSTALLATION" = "false" ] || { [ "$MAINTENANCE_CONFIGURED_BEFORE" = "true" ] && [ "$MAINTENANCE_POLICY_EXISTED" = "false" ]; }`) {
		t.Fatal("installer may overwrite policy during an unrelated upgrade or skip repair of a broken maintenance install")
	}
	if !strings.Contains(script, `chown root:root "$MAINTENANCE_HELPER_PATH"`) {
		t.Fatal("privileged helper is not protected from replacement by the Agent user")
	}
	if !strings.Contains(script, `trap 'on_installer_exit $?' 0`) || !strings.Contains(script, `trap 'exit 130' INT`) || !strings.Contains(script, `restore_transaction_file "$restore_path" "$restore_name" "$restore_changed"`) {
		t.Fatal("installer does not arm rollback for exit and interruption")
	}
	preflight := strings.Index(script, "prepare_transaction_quiesce || exit $?")
	transaction := strings.Index(script, "begin_install_transaction || exit 1")
	if preflight < 0 || transaction < 0 || preflight >= transaction {
		t.Fatal("transaction starts before maintenance preflight")
	}
	assetValidation := strings.Index(script, `Downloaded Agent does not report Beszel Plus v${INSTALL_VERSION}.`)
	if assetValidation < 0 || assetValidation >= preflight {
		t.Fatal("existing services may be quiesced before release assets are validated")
	}
	aptDecision := strings.Index(script, `diagnose-install --apt-required=true`)
	if aptDecision < 0 || aptDecision >= preflight {
		t.Fatal("required APT coordination must finish before Beszel services are quiesced")
	}
	if strings.Contains(script, "Upgrade not started: APT/dpkg is active") {
		t.Fatal("installer still aborts unconditionally when any APT process is active")
	}
	if strings.Contains(script, "systemctl list-units --all --no-legend 'beszel-maintenance@*.service' 2>/dev/null |") {
		t.Fatal("maintenance unit loop still runs in a POSIX pipeline subshell")
	}
	if strings.Contains(script, `cp -p "$TRANSACTION_DIR/$transaction_name" "$transaction_path"`) {
		t.Fatal("rollback still copies directly over an executable")
	}
	policy := strings.Index(script, `"operation":"apply-update-policy"`)
	startLog := strings.Index(script, `echo "Starting Beszel Plus Agent..."`)
	agentStart := -1
	if startLog >= 0 {
		agentStart = startLog + strings.Index(script[startLog:], `systemctl restart beszel-agent.service`)
	}
	if policy < 0 || agentStart < 0 || policy >= agentStart {
		t.Fatal("installer starts the Agent before applying the initial policy")
	}
	helperDownload := strings.Index(script, `echo "Downloading maintenance helper v${INSTALL_VERSION}..."`)
	agentInstallCall := -1
	if helperDownload >= 0 {
		agentInstallCall = strings.Index(script[helperDownload:], "install_agent_binary || exit 1")
	}
	if agentInstallCall < 0 {
		t.Fatal("installer installs the Agent before downloading and validating the helper")
	}
	if early := strings.Index(script[:policy], "systemctl enable --now apt-daily.timer apt-daily-upgrade.timer"); early >= 0 {
		t.Fatal("installer enables APT timers before policy validation")
	}
	socketCheck := strings.Index(script, `systemctl is-active --quiet beszel-maintenance.socket`)
	success := strings.LastIndex(script, "Beszel Plus Agent has been installed successfully")
	if socketCheck < 0 || success < 0 || socketCheck >= success || !strings.Contains(script[socketCheck:success], "exit 1") {
		t.Fatal("installer can report success without failing an inactive maintenance socket")
	}
}

func TestPortableTimeoutTerminatesLegacyHelper(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "run_with_portable_timeout() {")
	end := strings.Index(script[start:], "\n}\n\n# Check if running as root")
	if start < 0 || end < 0 {
		t.Fatal("portable timeout function not found")
	}
	function := script[start : start+end+3]
	dir := t.TempDir()
	helper := filepath.Join(dir, "legacy-helper")
	pidFile := filepath.Join(dir, "legacy.pid")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho $$ > \"$PID_FILE\"\ntrap 'exit 0' TERM\nwhile :; do :; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cmd := exec.Command("sh", "-c", function+"\nrun_with_portable_timeout 1 \"$LEGACY_HELPER\" --version")
	cmd.Env = append(os.Environ(), "LEGACY_HELPER="+helper, "PID_FILE="+pidFile)
	_ = cmd.Run()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("legacy helper detection exceeded timeout: %v", elapsed)
	}
	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", "kill -0 \"$1\" 2>/dev/null", "sh", strings.TrimSpace(string(pid))).Run(); err == nil {
		t.Fatal("portable timeout left the legacy helper running")
	}
}

func TestLegacyHelperVersionDetectionClassifiesOutputs(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "run_with_portable_timeout() {")
	end := strings.Index(script[start:], "\n# Check if running as root")
	if start < 0 || end < 0 {
		t.Fatal("helper detection functions not found")
	}
	functions := script[start : start+end]
	for _, test := range []struct {
		name, body, want string
	}{
		{"valid", "printf 'beszel-maintenance-helper v0.1.2\\nprotocol 1\\n'\n", "RESULT=0.1.2"},
		{"invalid", "printf 'unknown output\\n'\n", "RESULT=legacy"},
		{"error", "exit 1\n", "RESULT=legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := filepath.Join(dir, "helper")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\n"+test.body), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", functions+"\ndetect_existing_helper_version \"$HELPER\"\nprintf 'RESULT=%s\\n' \"$OLD_HELPER_VERSION\"")
			cmd.Env = append(os.Environ(), "HELPER="+helper)
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("output=%q err=%v, want %q", output, err, test.want)
			}
		})
	}
}

func TestInstallerRollbackRestoresAgentAndHelper(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "append_component() {")
	end := strings.Index(script[start:], "\nEXISTING_INSTALLATION=false")
	if start < 0 || end < 0 {
		t.Fatal("transaction functions not found")
	}
	functions := script[start : start+end]
	dir := t.TempDir()
	agent := filepath.Join(dir, "beszel-agent")
	helper := filepath.Join(dir, "maintenance-helper")
	newAgent := filepath.Join(dir, "new-agent")
	newHelper := filepath.Join(dir, "new-helper")
	transaction := filepath.Join(dir, "transaction")
	for path, contents := range map[string]string{agent: "old-agent", helper: "old-helper", newAgent: "new-agent", newHelper: "new-helper"} {
		if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(transaction, 0o700); err != nil {
		t.Fatal(err)
	}
	harness := `
is_alpine() { return 0; }
is_openwrt() { return 1; }
is_freebsd() { return 1; }
` + functions + `
TRANSACTION_ACTIVE=true
QUIESCE_ACTIVE=false
EXISTING_INSTALLATION=true
AGENT_REPLACED=true
HELPER_REPLACED=true
AGENT_SERVICE_CHANGED=false
SOCKET_CHANGED=false
TEMPLATE_CHANGED=false
MONITORING_DROPIN_CHANGED=false
CONNECTION_DROPIN_CHANGED=false
UPDATE_SERVICE_CHANGED=false
UPDATE_TIMER_CHANGED=false
ROLLBACK_FAILED_COMPONENTS=""
ROLLBACK_RESTORED_COMPONENTS=""
INSTALL_VERSION=0.2.1
OLD_AGENT_VERSION=0.2.0
ROLLBACK_REPORT_PATH="$TRANSACTION_DIR/rollback-report.json"
TEMP_DIR=""
systemctl() { return 0; }
service_state() { echo inactive; }
restore_quiesced_services() { return 0; }
backup_transaction_file "$BIN_PATH" agent
backup_transaction_file "$MAINTENANCE_HELPER_PATH" helper
cp "$NEW_AGENT" "$BIN_PATH"
cp "$NEW_HELPER" "$MAINTENANCE_HELPER_PATH"
rollback_install 1
`
	cmd := exec.Command("sh", "-c", harness)
	cmd.Env = append(os.Environ(), "BIN_PATH="+agent, "MAINTENANCE_HELPER_PATH="+helper, "NEW_AGENT="+newAgent, "NEW_HELPER="+newHelper, "TRANSACTION_DIR="+transaction)
	if err := cmd.Run(); err == nil {
		t.Fatal("rollback harness unexpectedly reported success")
	}
	for path, want := range map[string]string{agent: "old-agent", helper: "old-helper"} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("%s was not restored: %q, %v", path, contents, err)
		}
	}
	reportData, err := os.ReadFile(filepath.Join(transaction, "rollback-report.json"))
	if err != nil {
		t.Fatal("rollback report missing:", err)
	}
	var report struct {
		Restored []string `json:"restored_components"`
		Failed   []string `json:"failed_components"`
	}
	if err := json.Unmarshal(reportData, &report); err != nil {
		t.Fatalf("invalid rollback report: %v: %s", err, reportData)
	}
	if len(report.Failed) != 0 || len(report.Restored) != 2 {
		t.Fatalf("unexpected rollback report: %#v", report)
	}
}

func TestMaintenancePreflightClassifiesProtocolOperation(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "json_string_value() {")
	end := strings.Index(script[start:], "\nrestore_transaction_file() {")
	if start < 0 || end < 0 {
		t.Fatal("preflight functions not found")
	}
	functions := script[start : start+end]
	for _, tc := range []struct {
		name, operation string
		wantStatus      int
		wantStop        bool
	}{
		{name: "critical upgrade", operation: "run-unattended-upgrades", wantStatus: 75},
		{name: "transactional policy", operation: "apply-update-policy", wantStatus: 75},
		{name: "cancelable validation", operation: "run-update-dry-run", wantStatus: 0, wantStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			statusPath := filepath.Join(dir, "status.json")
			stopPath := filepath.Join(dir, "stopped")
			if err := os.WriteFile(statusPath, []byte(`{"operation":"`+tc.operation+`","request_id":"request-123","status":"running","started_at":"2026-07-14T10:00:00Z"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			harness := functions + `
systemctl() {
  if [ "$1" = "list-units" ]; then echo "beszel-maintenance@1.service loaded active running"; return 0; fi
  if [ "$1" = "stop" ]; then : > "$STOP_PATH"; return 0; fi
  if [ "$1" = "show" ]; then
    property=""
    while [ $# -gt 0 ]; do [ "$1" = "-p" ] && { shift; property="$1"; }; shift; done
    if [ -f "$STOP_PATH" ] && [ "$property" = "ActiveState" ]; then echo inactive; return 0; fi
    case "$property" in ActiveState) echo active ;; SubState) echo running ;; MainPID|ExecMainPID) echo 4242 ;; ActiveEnterTimestamp) echo "Tue 2026-07-14 10:00:00 UTC" ;; esac
  fi
}
readlink() { echo "$MAINTENANCE_HELPER_PATH"; }
cat() { case "$1" in /proc/*/cgroup) echo "0::/system.slice/beszel-maintenance@1.service" ;; *) command cat "$@" ;; esac; }
maintenance_preflight
`
			cmd := exec.Command("sh", "-c", harness)
			cmd.Env = append(os.Environ(),
				"MAINTENANCE_STATUS_PATH="+statusPath,
				"MAINTENANCE_HELPER_PATH=/usr/local/libexec/beszel/maintenance-helper",
				"STOP_PATH="+stopPath,
				"WAIT_FOR_MAINTENANCE=1",
				"VERBOSE=false",
			)
			output, runErr := cmd.CombinedOutput()
			status := 0
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				status = exitErr.ExitCode()
			} else if runErr != nil {
				t.Fatal(runErr)
			}
			if status != tc.wantStatus {
				t.Fatalf("status=%d output=%s", status, output)
			}
			if tc.wantStatus == 75 && (!strings.Contains(string(output), tc.operation) || !strings.Contains(string(output), "No binaries or configuration files were changed")) {
				t.Fatalf("blocking diagnostic is incomplete: %s", output)
			}
			_, stopErr := os.Stat(stopPath)
			if tc.wantStop != (stopErr == nil) {
				t.Fatalf("stop=%v want=%v output=%s", stopErr == nil, tc.wantStop, output)
			}
		})
	}
}

func TestInstallerManagementFlagsAreTriState(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, `OS_UPDATE_MANAGEMENT_FLAG=""`) || !strings.Contains(script, `POWER_MANAGEMENT_FLAG=""`) {
		t.Fatal("management flags do not have an unset state")
	}
	for _, required := range []string{
		"preserving existing setting: $OS_UPDATE_MANAGEMENT_FLAG",
		"preserving existing setting: $POWER_MANAGEMENT_FLAG",
		"read_existing_management_flag OS_UPDATE_MANAGEMENT",
		"read_existing_management_flag POWER_MANAGEMENT",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("tri-state preservation is missing %q", required)
		}
	}
}

func TestAtomicRollbackWorksWhilePreviousExecutableIsRunning(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "restore_transaction_file() {")
	end := strings.Index(script[start:], "\nrollback_install() {")
	if start < 0 || end < 0 {
		t.Fatal("atomic restore function not found")
	}
	function := script[start : start+end]
	dir := t.TempDir()
	target := filepath.Join(dir, "beszel-agent")
	transaction := filepath.Join(dir, "transaction")
	if err := os.Mkdir(transaction, 0o700); err != nil {
		t.Fatal(err)
	}
	oldBinary, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Skip("/bin/sleep unavailable")
	}
	newBinary, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skip("/bin/true unavailable")
	}
	if err := os.WriteFile(target, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction, "agent"), oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	running := exec.Command(target, "10")
	if err := running.Start(); err != nil {
		t.Skipf("cannot execute fixture: %v", err)
	}
	defer func() { _ = running.Process.Kill(); _ = running.Wait() }()
	replacement := target + ".new"
	if err := os.WriteFile(replacement, newBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, target); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", function+"\n"+`restore_transaction_file "$BIN_PATH" agent true`)
	cmd.Env = append(os.Environ(), "BIN_PATH="+target, "TRANSACTION_DIR="+transaction)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("atomic restore failed while executable was active: %v: %s", err, output)
	}
	restored, err := os.ReadFile(target)
	if err != nil || string(restored) != string(oldBinary) {
		t.Fatal("running executable was not restored atomically")
	}
}

func TestQuiesceRestoresOnlyPreviouslyActiveServices(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "restore_quiesced_services() {")
	end := strings.Index(script[start:], "\nclear_upgrade_drain() {")
	if start < 0 || end < 0 {
		t.Fatal("service restoration function not found")
	}
	function := script[start : start+end]
	for _, tc := range []struct {
		name, agent, socket, want string
	}{
		{"both active", "active", "active", "beszel-maintenance.socket\nbeszel-agent.service\n"},
		{"inactive preserved", "inactive", "inactive", ""},
		{"failed preserved", "failed", "failed", ""},
		{"only agent active", "active", "inactive", "beszel-agent.service\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "starts")
			harness := function + `
systemctl() { [ "$1" = "start" ] && printf '%s\n' "$2" >> "$CAPTURE"; return 0; }
QUIESCE_ACTIVE=true
restore_quiesced_services
`
			cmd := exec.Command("sh", "-c", harness)
			cmd.Env = append(os.Environ(), "AGENT_PREVIOUS_STATE="+tc.agent, "SOCKET_PREVIOUS_STATE="+tc.socket, "CAPTURE="+capture)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("restore failed: %v: %s", err, output)
			}
			got, _ := os.ReadFile(capture)
			if string(got) != tc.want {
				t.Fatalf("starts=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestInstallerLockRejectsConcurrentRun(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock unavailable")
	}
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "acquire_installer_lock() {")
	end := strings.Index(script[start:], "\nwrite_upgrade_drain() {")
	if start < 0 || end < 0 {
		t.Fatal("installer lock function not found")
	}
	function := script[start : start+end]
	lockPath := filepath.Join(t.TempDir(), "install.lock")
	env := append(os.Environ(), "INSTALL_LOCK_PATH="+lockPath, "LOCK_KIND=", "LOCK_DIR=")
	first := exec.Command("sh", "-c", function+"\nacquire_installer_lock || exit $?\necho ready\nsleep 30")
	first.Env = env
	stdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Process.Kill(); _ = first.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("first installer did not acquire lock")
	}
	second := exec.Command("sh", "-c", function+"\nacquire_installer_lock")
	second.Env = env
	output, err := second.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 75 || !strings.Contains(string(output), "already running") {
		t.Fatalf("concurrent installer was not rejected: err=%v output=%s", err, output)
	}
}

func TestMaintenanceSocketAcceptHasCorrespondingTemplate(t *testing.T) {
	socketPath := "../../supplemental/systemd/beszel-maintenance.socket"
	data, err := os.ReadFile(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(data)
	if !strings.Contains(unit, "Accept=yes") {
		t.Fatal("maintenance socket must use Accept=yes")
	}
	serviceName := "beszel-maintenance@.service"
	if match := regexp.MustCompile(`(?m)^Service=([^\s]+)$`).FindStringSubmatch(unit); len(match) == 2 {
		serviceName = match[1]
	}
	servicePath := filepath.Join(filepath.Dir(socketPath), serviceName)
	if !strings.Contains(serviceName, "@.service") {
		t.Fatalf("socket service %q is not a template", serviceName)
	}
	if _, err := os.Stat(servicePath); err != nil {
		t.Fatalf("socket template %q is missing: %v", servicePath, err)
	}
}

func TestInstallerExecutesConfiguredHelperForInitialPolicy(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^\s*if ! POLICY_RESPONSE=\$\((printf .* \| "\$MAINTENANCE_HELPER_PATH")\); then$`).FindSubmatch(data)
	if len(match) != 2 {
		t.Fatal("initial policy helper pipeline not found or not safely expanded")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "request.json")
	helper := filepath.Join(dir, "maintenance-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncat > \"$HELPER_CAPTURE\"\nprintf '{\"status\":\"completed\"}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", string(match[1]))
	cmd.Env = append(os.Environ(), "MAINTENANCE_HELPER_PATH="+helper, "HELPER_CAPTURE="+capture, "POLICY_MODE=security")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initial policy pipeline failed: %v: %s", err, output)
	}
	request, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal("configured helper was not executed:", err)
	}
	if !strings.Contains(string(request), `"operation":"apply-update-policy"`) || !strings.Contains(string(request), `"mode":"security"`) {
		t.Fatalf("unexpected helper request: %s", request)
	}
}
