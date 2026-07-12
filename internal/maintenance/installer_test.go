package maintenance

import (
	"os"
	"strings"
	"testing"
)

func TestInstallerContainsSafeMaintenanceUpgradeFlow(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-agent.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{"--agent-auto-update", "--os-update-management", "--os-update-policy", "EXISTING_INSTALLATION", "beszel-maintenance.socket", "SocketMode=0660", "automatic_reboot\":false", "KEY=", "TOKEN=", "HUB_URL=", "/usr/local/libexec/beszel/maintenance-helper"} {
		if !strings.Contains(script, required) {
			t.Fatalf("installer missing %q", required)
		}
	}
	for _, forbidden := range []string{"systemctl reboot", "shutdown -r", "reboot now"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("installer contains reboot command %q", forbidden)
		}
	}
	if !strings.Contains(script, `if [ "$EXISTING_INSTALLATION" = "false" ]`) {
		t.Fatal("installer may apply a new APT policy during an upgrade")
	}
	if !strings.Contains(script, `chown root:root "$MAINTENANCE_HELPER_PATH"`) {
		t.Fatal("privileged helper is not protected from replacement by the Agent user")
	}
}
