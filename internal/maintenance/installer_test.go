package maintenance

import (
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
	for _, required := range []string{"10-beszel-connection.conf", "maintenance-smoke", "TRANSACTION_DIR", "rollback_install", "Maintenance helper protocol compatibility verified", `"retryable":true`, "cleanup_stale_maintenance_validations", "operation=run-update-dry-run", "non-validation Beszel Plus maintenance operation is active"} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing upgrade safeguard %q", required)
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
	if !strings.Contains(script, `trap 'rollback_install $?' 0`) || !strings.Contains(script, `trap 'exit 130' INT`) || !strings.Contains(script, `restore_transaction_file "$BIN_PATH" agent`) {
		t.Fatal("installer does not arm rollback for exit and interruption")
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
	start := strings.Index(script, "backup_transaction_file() {")
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
AGENT_WAS_RUNNING=true
SOCKET_WAS_RUNNING=false
EXISTING_INSTALLATION=true
TEMP_DIR=""
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
	if _, err := os.Stat(transaction); !os.IsNotExist(err) {
		t.Fatalf("transaction files were not cleaned: %v", err)
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
