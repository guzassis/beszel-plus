package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const dracutFailureLog = `realpath: /lib/modules/7.0.0-fixture: No such file or directory
dracut[F]: Cannot find module directory
dracut[F]: and --no-kernel was not specified
update-initramfs: failed for /boot/initrd.img-fixture with 1.
dpkg: error processing package dracut (--configure):
 old dracut package postinst maintainer script subprocess failed with exit status 1
`

func TestInstallFailureDiagnosticsUseOnlyCurrentLogAppend(t *testing.T) {
	for _, initial := range []bool{false, true} {
		h := testHelper(t)
		path := h.path("/var/log/apt/term.log")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if initial {
			fixtureWrite(t, path, "dpkg: error processing package historical-failure (--configure):\n", 0o600)
		}
		cursors := h.installLogCursors()
		if initial {
			if summary := installFailureSummary(cursors); summary != "" {
				t.Fatalf("historic failure leaked: %s", summary)
			}
			// The open descriptor continues to follow the installation's log.
			if err := os.Rename(path, path+".1"); err != nil {
				t.Fatal(err)
			}
			path += ".1"
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		file.WriteString(strings.Repeat("progress\n", maxInstallLogBytes) + dracutFailureLog)
		file.Close()
		summary := installFailureSummary(cursors)
		closeInstallLogs(cursors)
		if !strings.Contains(summary, "package dracut") || !strings.Contains(summary, "/lib/modules/7.0.0-fixture") || strings.Contains(summary, "historical-failure") || len(summary) > 470 {
			t.Fatalf("wrong current bounded diagnostic: %s", summary)
		}
	}
}

func TestCycleInstallationFailurePersistsDpkgCause(t *testing.T) {
	policy := entity.DefaultPolicy()
	h, runner := cycleScenario(t, policy)
	var before aptInventory
	if err := json.Unmarshal([]byte(`{"packages":`+packagesJSON(120)+`}`), &before); err != nil {
		t.Fatal(err)
	}
	before.Packages = append(before.Packages, inventoryPackage{Name: "unknown-source", Installed: "1.0", NewerAvailable: true, Held: boolPointer(false)})
	runner.before = &before
	path := h.path("/var/log/apt/term.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Runner = helperRunnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "unattended-upgrade" && len(args) == 1 && args[0] == "--verbose" {
			fixtureWrite(t, path, dracutFailureLog, 0o600)
			return []byte("NoneType: None\ninstallArchives() failed\nPackage sg3-utils is kept back"), errors.New("exit status 1")
		}
		return runner.Run(ctx, name, args...)
	})
	cycle := responseCycleForTest(t, h.Execute(context.Background(), cycleRequest(policy)))
	if cycle.State != entity.CycleFailed || cycle.Stage != entity.CycleStageInstall || cycle.ErrorCode != "command_failed" || !strings.Contains(cycle.Error, "package dracut") || !strings.Contains(cycle.Error, "/lib/modules/7.0.0-fixture") || cycle.Verified || !strings.Contains(cycle.VerificationMessage, "unknown eligibility") {
		t.Fatalf("root cause was not preserved in failed cycle: %#v", cycle)
	}
}
