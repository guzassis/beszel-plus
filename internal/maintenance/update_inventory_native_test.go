package maintenance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the shipped script and ExecRunner against distribution python3-apt,
// using temporary lists/status only. No downloads or package changes occur.
func TestAPTInventoryWithDistributionBindings(t *testing.T) {
	t.Setenv("PYTHONPATH", "")
	t.Setenv("PYTHONHOME", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	probe := exec.CommandContext(ctx, "/usr/bin/python3", "-c", "import apt, apt_pkg; print(apt.__file__, apt_pkg.VERSION)")
	if output, err := probe.CombinedOutput(); err != nil {
		if os.Getenv("BESZEL_TEST_APT") == "1" {
			t.Fatalf("distribution python3-apt is required: %v: %s", err, output)
		}
		t.Skipf("distribution python3-apt is unavailable: %v: %s", err, output)
	} else {
		t.Logf("real APT bindings: %s", strings.TrimSpace(string(output)))
	}

	root := t.TempDir()
	for _, dir := range []string{"etc/apt/apt.conf.d", "etc/apt/sources.list.d", "etc/apt/preferences.d", "var/lib/dpkg", "var/lib/apt/lists/partial", "var/cache/apt/archives/partial"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("apt.conf", fmt.Sprintf(`Dir %q;
Dir::State::status %q;
Dir::Cache::pkgcache "";
Dir::Cache::srcpkgcache "";
APT::Architecture "amd64";
APT::Architectures { "amd64"; "i386"; };
`, root, filepath.Join(root, "var/lib/dpkg/status")))
	t.Setenv("APT_CONFIG", filepath.Join(root, "apt.conf"))
	write("etc/apt/sources.list", "deb [trusted=yes] http://fixture.invalid/ubuntu noble main\n")
	write("etc/apt/preferences", `Package: pinned-package
Pin: version 2.0
Pin-Priority: -1

Package: pinned-downgrade
Pin: version 1.9
Pin-Priority: 1001
`)
	write("var/lib/apt/lists/fixture.invalid_ubuntu_dists_noble_Release", "Origin: Ubuntu\nLabel: Ubuntu\nSuite: noble\nCodename: noble\nArchitectures: amd64 i386\nComponents: main\n")
	var status strings.Builder
	lists := map[string]*strings.Builder{"amd64": {}, "i386": {}}
	add := func(name, arch, installed, candidate string, held bool, extra ...string) {
		if installed != "" {
			selection := "install"
			if held {
				selection = "hold"
			}
			fmt.Fprintf(&status, "Package: %s\nStatus: %s ok installed\nArchitecture: %s\nMulti-Arch: same\nVersion: %s\nDescription: inventory fixture\n\n", name, selection, arch, installed)
		}
		for _, version := range append([]string{candidate}, extra...) {
			fmt.Fprintf(lists[arch], "Package: %s\nArchitecture: %s\nMulti-Arch: same\nVersion: %s\nFilename: pool/%s_%s.deb\nSize: 1\nDescription: inventory fixture\n\n", name, arch, version, name, arch)
		}
	}
	for i := range 500 {
		add(fmt.Sprintf("candidate-%03d", i), "amd64", "1.0", "2.0", false)
	}
	add("held-package", "amd64", "1.0", "2.0", true)
	add("pinned-package", "amd64", "1.0", "2.0", false)
	add("epoch-tilde-upgrade", "amd64", "1:2.0~rc1-1", "1:2.0-1", false)
	add("revision-upgrade", "amd64", "1.0-1", "1.0-2", false)
	add("pinned-downgrade", "amd64", "2.0", "1.9", false, "2.1")
	add("multiarch", "amd64", "1.0", "2.0", false)
	add("multiarch", "i386", "1.0", "2.0", false)
	add("not-installed", "amd64", "", "2.0", false)
	add("unchanged", "amd64", "2.0", "2.0", false)
	add("downgrade-only", "amd64", "2.0", "1.0", false)
	write("var/lib/dpkg/status", status.String())
	for arch, packages := range lists {
		write("var/lib/apt/lists/fixture.invalid_ubuntu_dists_noble_main_binary-"+arch+"_Packages", packages.String())
	}

	h := testHelper(t)
	h.Runner = ExecRunner{}
	inventory, err := h.readAPTInventory(ctx)
	if err != nil {
		t.Fatalf("shipped inventory failed with real python3-apt: %v", err)
	}
	if len(inventory.Packages) != 507 {
		t.Fatalf("got %d candidates, want all 507 including both architectures", len(inventory.Packages))
	}
	packages := make(map[string]inventoryPackage)
	for _, pkg := range inventory.Packages {
		if _, duplicate := packages[pkg.Name]; duplicate {
			t.Fatalf("duplicate package identity: %s", pkg.Name)
		}
		packages[pkg.Name] = pkg
	}
	for _, name := range []string{"candidate-000", "candidate-499", "multiarch", "multiarch:i386", "epoch-tilde-upgrade", "revision-upgrade"} {
		pkg := packages[name]
		if pkg.CandidateNewer == nil || !*pkg.CandidateNewer || !pkg.NewerAvailable || pkg.Priority != 500 || pkg.Held == nil || *pkg.Held {
			t.Fatalf("missing candidate metadata for %s: %#v", name, pkg)
		}
		if len(pkg.Origins) != 1 || pkg.Origins[0] != (inventoryOrigin{Origin: "Ubuntu", Label: "Ubuntu", Codename: "noble", Archive: "noble", Site: "fixture.invalid", Trusted: true}) {
			t.Fatalf("incorrect real APT origin for %s: %#v", name, pkg.Origins)
		}
	}
	if pkg := packages["held-package"]; pkg.Held == nil || !*pkg.Held {
		t.Fatalf("real dpkg hold lost: %#v", pkg)
	}
	for name, candidate := range map[string]string{"pinned-package": "1.0", "pinned-downgrade": "1.9"} {
		pkg := packages[name]
		if pkg.Candidate != candidate || pkg.CandidateNewer == nil || *pkg.CandidateNewer || !pkg.NewerAvailable {
			t.Fatalf("real APT pin/version comparison lost for %s: %#v", name, pkg)
		}
	}
	for _, name := range []string{"not-installed", "unchanged", "downgrade-only"} {
		if _, included := packages[name]; included {
			t.Errorf("%s is not an installed update candidate", name)
		}
	}
	t.Log("inventoried 507 candidates with real holds, pins, Debian version comparison and multiarch identities")
}
