package maintenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	for _, required := range []string{"10-beszel-connection.conf", "maintenance-smoke", "HELPER_BACKUP_PATH", "Maintenance helper protocol compatibility verified", `"retryable":true`} {
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
	socketCheck := strings.Index(script, `systemctl is-active --quiet beszel-maintenance.socket`)
	success := strings.LastIndex(script, "Beszel Agent has been installed successfully")
	if socketCheck < 0 || success < 0 || socketCheck >= success || !strings.Contains(script[socketCheck:success], "exit 1") {
		t.Fatal("installer can report success without failing an inactive maintenance socket")
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
