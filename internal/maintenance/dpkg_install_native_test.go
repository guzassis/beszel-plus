package maintenance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The debs below belong only to an isolated dpkg database. Their maintainer
// script generates a real initramfs for a synthetic kernel; no host package,
// boot image, firewall rule or loaded kernel module is changed.
type dpkgFixture struct {
	root, modules, kernel string
	debs                  []string
}

func installFixture(t *testing.T, native bool) dpkgFixture {
	t.Helper()
	for _, command := range []string{"dpkg", "dpkg-deb", "dracut", "cpio"} {
		if _, err := exec.LookPath(command); err != nil {
			if os.Getenv("BESZEL_TEST_APT") == "1" || native {
				t.Fatalf("integration dependency %s is required: %v", command, err)
			}
			t.Skipf("integration dependency %s is unavailable", command)
		}
	}
	f := dpkgFixture{root: t.TempDir()}
	if native {
		f.root = strings.TrimSpace(string(fixtureCommand(t, "sudo", "-n", "mktemp", "-d", "/run/beszel-dpkg-test-XXXXXXXX")))
		t.Cleanup(func() { fixtureCommand(t, "sudo", "-n", "rm", "-rf", "--", f.root) })
		fixtureCommand(t, "sudo", "-n", "chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), f.root)
	}
	f.kernel = "1.0.0-" + filepath.Base(f.root)
	f.modules = filepath.Join(f.root, "lib/modules", f.kernel)
	if native {
		f.modules = filepath.Join("/usr/lib/modules", f.kernel)
		// mkdir fails rather than taking ownership of an existing directory.
		fixtureCommand(t, "sudo", "-n", "mkdir", "--", f.modules)
		t.Cleanup(func() { fixtureCommand(t, "sudo", "-n", "rm", "-rf", "--", f.modules) })
		fixtureCommand(t, "sudo", "-n", "chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), f.modules)
	} else if err := os.MkdirAll(f.modules, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"var/lib/dpkg/updates", "tmp", "conf", "boot"} {
		if err := os.MkdirAll(filepath.Join(f.root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"modules.dep", "modules.builtin", "modules.order", "modules.builtin.modinfo"} {
		fixtureWrite(t, filepath.Join(f.modules, name), "", 0o644)
	}
	for _, version := range []string{"1.0", "2.0"} {
		stage := filepath.Join(f.root, "package-"+version)
		if err := os.MkdirAll(filepath.Join(stage, "DEBIAN"), 0o755); err != nil {
			t.Fatal(err)
		}
		fixtureWrite(t, filepath.Join(stage, "DEBIAN/control"), fmt.Sprintf("Package: beszel-initramfs-fixture\nVersion: %s\nArchitecture: all\nMaintainer: Beszel tests <tests@example.invalid>\nDescription: isolated initramfs upgrade regression\n", version), 0o644)
		if version == "2.0" {
			probe := ""
			if native {
				probe = `/usr/bin/python3 - <<'PY'
import socket
status = dict(line.strip().split(":", 1) for line in open("/proc/self/status") if ":" in line)
assert int(status["CapEff"], 16) & (1 << 16) == 0, "CAP_SYS_MODULE was granted"
assert int(status["NoNewPrivs"]) == 1
assert int(status["Seccomp"]) == 2
with socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, socket.NETLINK_ROUTE):
    pass
print("module load denied; netlink available; sandbox active")
PY
`
			}
			postinst := fmt.Sprintf(`#!/bin/sh
set -eu
[ "$1" = configure ] || exit 0
realpath -e '%s'
%s
dracut --conf /dev/null --confdir "$DPKG_ROOT/conf" --tmpdir "$DPKG_ROOT/tmp" \
  --kmoddir '%s' --modules base --no-hostonly --no-early-microcode --no-compress --nostrip \
  --kver '%s' --force "$DPKG_ROOT/boot/initramfs.img"
printf '2.0\n' > "$DPKG_ROOT/upgrade-complete"
`, f.modules, probe, f.modules, f.kernel)
			fixtureWrite(t, filepath.Join(stage, "DEBIAN/postinst"), postinst, 0o755)
		}
		deb := filepath.Join(f.root, "fixture-"+version+".deb")
		fixtureCommand(t, "dpkg-deb", "--build", stage, deb)
		f.debs = append(f.debs, deb)
	}
	return f
}

func fixtureWrite(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
}

func fixtureCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v: %s", name, args, err, output)
	}
	return output
}

func (f dpkgFixture) args(action ...string) []string {
	return append([]string{"--force-not-root", "--force-script-chrootless", "--root=" + f.root, "--log=" + filepath.Join(f.root, "dpkg.log")}, action...)
}

func (f dpkgFixture) verify(t *testing.T) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(f.root, "upgrade-complete")); err != nil || string(data) != "2.0\n" {
		t.Fatalf("upgrade maintainer script did not complete: %q: %v", data, err)
	}
	status, err := os.ReadFile(filepath.Join(f.root, "var/lib/dpkg/status"))
	if err != nil || !strings.Contains(string(status), "Status: install ok installed") || !strings.Contains(string(status), "Version: 2.0") {
		t.Fatalf("upgraded package was not configured: %s: %v", status, err)
	}
	image, err := os.Open(filepath.Join(f.root, "boot/initramfs.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	cmd := exec.Command("cpio", "--list", "--quiet")
	cmd.Stdin = image
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "init") {
		t.Fatalf("dracut did not produce a valid initramfs archive: %v: %s", err, output)
	}
	if out := fixtureCommand(t, "dpkg", f.args("--audit")...); len(out) != 0 {
		t.Fatalf("isolated dpkg audit failed: %s", out)
	}
}

func TestDPKGUpgradeGeneratesRealInitramfs(t *testing.T) {
	f := installFixture(t, false)
	fixtureCommand(t, "dpkg", f.args("--install", f.debs[0])...)
	fixtureCommand(t, "dpkg", f.args("--install", f.debs[1])...)
	f.verify(t)
	t.Log("real dpkg upgraded 1.0 to 2.0, ran dracut and passed isolated audit")
}

func TestSystemdMaintenanceSandboxInstallsInitramfsUpgrade(t *testing.T) {
	if os.Getenv("BESZEL_TEST_SYSTEMD") != "1" {
		t.Skip("set BESZEL_TEST_SYSTEMD=1 on a disposable native systemd host with passwordless sudo")
	}
	f := installFixture(t, true)
	fixtureCommand(t, "sudo", append([]string{"-n", "dpkg"}, f.args("--install", f.debs[0])...)...)
	run := func(legacy bool, action ...string) ([]byte, error) {
		properties := shippedHelperProperties(t)
		if legacy {
			properties["ProtectKernelModules"] = "yes"
			properties["RestrictAddressFamilies"] = "AF_UNIX AF_INET AF_INET6"
			delete(properties, "CapabilityBoundingSet")
			delete(properties, "SystemCallFilter")
		}
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		args := []string{"-n", "systemd-run", "--quiet", "--wait", "--pipe", "--collect"}
		for _, key := range keys {
			args = append(args, "-p", key+"="+properties[key])
		}
		args = append(args, "/usr/bin/dpkg")
		args = append(args, f.args(action...)...)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return exec.CommandContext(ctx, "sudo", args...).CombinedOutput()
	}
	output, err := run(true, "--install", f.debs[1])
	if err == nil || !strings.Contains(string(output), f.modules) || !strings.Contains(string(output), "error processing package beszel-initramfs-fixture") {
		t.Fatalf("legacy service did not reproduce invisible module files during real dpkg configuration: %v: %s", err, output)
	}
	if output, err := run(false, "--configure", "beszel-initramfs-fixture"); err != nil {
		t.Fatalf("fixed service could not configure the upgrade: %v: %s", err, output)
	} else if !strings.Contains(string(output), "module load denied; netlink available; sandbox active") {
		t.Fatalf("maintainer script did not verify retained protections: %s", output)
	}
	fixtureCommand(t, "sudo", "-n", "chown", "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), f.root)
	f.verify(t)
	t.Log("legacy sandbox failed real dpkg installation; fixed sandbox rebuilt initramfs without module loading privileges")
}
