package maintenance

import (
	"os"
	"strings"
	"testing"
)

func TestHubInstallerHasTransactionalNativeFlow(t *testing.T) {
	data, err := os.ReadFile("../../supplemental/scripts/install-hub.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, required := range []string{
		`PRODUCT_VERSION="0.2.9"`,
		"beszel-plus-hub-install.lock",
		"sha256sum",
		"Checksum entry is missing or invalid",
		"Downloaded Hub version does not match",
		"TRANSACTION_ACTIVE=true",
		"rollback()",
		"Hub rollback completed and validated",
		`mv -f "${BIN_PATH}.new.$$" "$BIN_PATH"`,
		"systemd-analyze verify",
		"systemctl is-active --quiet beszel-hub.service",
		"UPDATE_TIMER_PREVIOUS_ENABLED",
		"UPDATE_TIMER_PREVIOUS_ACTIVE",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Hub installer missing safeguard %q", required)
		}
	}
	if strings.Contains(script, "apt-get") || strings.Contains(script, "dpkg ") {
		t.Fatal("Hub installer must not mutate package-manager state for bootstrap tools")
	}
	if strings.Contains(script, `${AUTO_UPDATE_FLAG:+`) {
		t.Fatal("Hub unit validation incorrectly treats false as an enabled update timer")
	}
}

func TestInstallersUseSafeSudoReexecAndDoNotTerminateAPT(t *testing.T) {
	for _, path := range []string{
		"../../supplemental/scripts/install-agent.sh",
		"../../supplemental/scripts/install-hub.sh",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		script := string(data)
		if !strings.Contains(script, `exec sudo -- "$0" "$@"`) {
			t.Errorf("%s does not preserve arguments through sudo safely", path)
		}
		for _, forbidden := range []string{"pkill apt", "pkill dpkg", "killall apt", "killall dpkg", "systemctl stop unattended-upgrades.service"} {
			if strings.Contains(script, forbidden) {
				t.Errorf("%s may terminate package management through %q", path, forbidden)
			}
		}
	}
}
