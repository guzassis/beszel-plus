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

	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

func suiteOptionalPackage(name string, origin inventoryOrigin) inventoryPackage {
	return inventoryPackage{
		Name:           name,
		Installed:      "1.0",
		Candidate:      "2.0",
		NewerAvailable: true,
		CandidateNewer: boolPointer(true),
		Priority:       500,
		Held:           boolPointer(false),
		Origins:        []inventoryOrigin{origin},
	}
}

func classifySuiteOptional(t *testing.T, policy entity.Policy, pkg inventoryPackage, installable map[string]bool, repositories []entity.Repository) inventoryResult {
	t.Helper()
	h := testHelper(t)
	if err := os.WriteFile(h.path("/etc/os-release"), []byte("ID=ubuntu\nVERSION_CODENAME=resolute\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h.classifyInventory(aptInventory{Packages: []inventoryPackage{pkg}}, policy, installable, nil, true, repositories)
}

func TestVendorWithoutSuiteIsExcludedAndValidForBuiltInPolicies(t *testing.T) {
	origin := inventoryOrigin{
		Origin: "Tailscale", Label: "Tailscale", Codename: "resolute",
		Site: "pkgs.tailscale.com", Trusted: true,
	}
	pkg := suiteOptionalPackage("tailscale", origin)
	for _, mode := range []entity.PolicyMode{entity.ModeOfficialAll, entity.ModeSecurity} {
		t.Run(string(mode), func(t *testing.T) {
			policy := entity.DefaultPolicy()
			policy.Mode = mode
			result := classifySuiteOptional(t, policy, pkg, map[string]bool{"tailscale": true}, nil)
			if !result.Valid || result.Counts.Excluded != 1 || result.Counts.Unknown != 0 || result.Counts.Eligible != 0 {
				t.Fatalf("vendor without Suite should be known and excluded: %#v", result)
			}
			if got := result.Counts.Packages[0].State; got != "excluded" {
				t.Fatalf("vendor without Suite state = %q, want excluded", got)
			}
		})
	}
}

func TestCustomVendorWithoutSuiteNeedsExplicitAuthorizationAndMatchingSite(t *testing.T) {
	origin := inventoryOrigin{
		Origin: "Tailscale", Label: "Tailscale", Codename: "bookworm",
		Site: "pkgs.tailscale.com", Trusted: true,
	}
	repository := entity.Repository{
		ID: "tailscale-repo", Origin: origin.Origin, Label: origin.Label,
		Codename: origin.Codename, Site: origin.Site, Trusted: true,
	}
	pkg := suiteOptionalPackage("tailscale", origin)

	policy := entity.DefaultPolicy()
	policy.Mode = entity.ModeCustom
	policy.ConfirmThirdParty = true
	policy.AllowedRepositories = []string{repository.ID}
	result := classifySuiteOptional(t, policy, pkg, map[string]bool{"tailscale": true}, []entity.Repository{repository})
	if !result.Valid || result.Counts.Eligible != 1 {
		t.Fatalf("explicitly authorized vendor without Suite was not eligible: %#v", result)
	}

	policy.ConfirmThirdParty = false
	result = classifySuiteOptional(t, policy, pkg, map[string]bool{"tailscale": true}, []entity.Repository{repository})
	if result.Counts.Eligible != 0 || result.Counts.Excluded != 1 {
		t.Fatalf("unconfirmed third-party repository was eligible: %#v", result)
	}

	policy.ConfirmThirdParty = true
	untrustedPackage := pkg
	untrustedPackage.Origins = append([]inventoryOrigin(nil), pkg.Origins...)
	untrustedPackage.Origins[0].Trusted = false
	result = classifySuiteOptional(t, policy, untrustedPackage, map[string]bool{"tailscale": true}, []entity.Repository{repository})
	if result.Counts.Eligible != 0 || result.Counts.Excluded != 1 {
		t.Fatalf("untrusted third-party repository was eligible: %#v", result)
	}

	policy.AllowedRepositories = []string{"different-repository-id"}
	result = classifySuiteOptional(t, policy, pkg, map[string]bool{"tailscale": true}, []entity.Repository{repository})
	if result.Counts.Eligible != 0 || result.Counts.Excluded != 1 {
		t.Fatalf("unselected repository ID was eligible: %#v", result)
	}

	policy.AllowedRepositories = []string{repository.ID}
	otherSite := repository
	otherSite.Site = "mirror.example"
	result = classifySuiteOptional(t, policy, pkg, map[string]bool{"tailscale": true}, []entity.Repository{otherSite})
	if result.Counts.Eligible != 0 || result.Counts.Unknown != 1 || result.Valid {
		t.Fatalf("repository from a different site inherited authorization: %#v", result)
	}
}

func TestOfficialOriginWithoutEssentialIdentityRemainsUnknown(t *testing.T) {
	base := inventoryOrigin{
		Origin: "Ubuntu", Label: "Ubuntu", Codename: "resolute",
		Site: "archive.ubuntu.com", Trusted: true,
	}
	for _, field := range []string{"origin", "label", "codename", "site"} {
		t.Run(field, func(t *testing.T) {
			origin := base
			switch field {
			case "origin":
				origin.Origin = ""
			case "label":
				origin.Label = ""
			case "codename":
				origin.Codename = ""
			case "site":
				origin.Site = ""
			}
			policy := entity.DefaultPolicy()
			result := classifySuiteOptional(t, policy, suiteOptionalPackage("official", origin), map[string]bool{"official": true}, nil)
			if result.Valid || result.Counts.Unknown != 1 || result.Counts.Eligible != 0 {
				t.Fatalf("missing %s identity was not kept unknown: %#v", field, result)
			}
		})
	}
}

func TestOfficialCurrentReleaseWithoutSuiteRemainsAuthorized(t *testing.T) {
	policy := entity.DefaultPolicy()
	origin := inventoryOrigin{
		Origin: "Ubuntu", Label: "Ubuntu", Codename: "resolute",
		Site: "archive.ubuntu.com", Trusted: true,
	}
	result := classifySuiteOptional(t, policy, suiteOptionalPackage("ubuntu-base", origin), map[string]bool{"ubuntu-base": true}, nil)
	if !result.Valid || result.Counts.Eligible != 1 {
		t.Fatalf("official current release without Suite was not eligible: %#v", result)
	}
}

func TestSecurityDoesNotTreatEmptyArchiveBaseReleaseAsSecurity(t *testing.T) {
	policy := entity.DefaultPolicy()
	policy.Mode = entity.ModeSecurity
	origin := inventoryOrigin{
		Origin: "Ubuntu", Label: "Ubuntu", Codename: "resolute",
		Site: "security.ubuntu.com", Trusted: true,
	}
	result := classifySuiteOptional(t, policy, suiteOptionalPackage("base-release", origin), map[string]bool{"base-release": true}, nil)
	if !result.Valid || result.Counts.Excluded != 1 || result.Counts.Eligible != 0 {
		t.Fatalf("base release with empty Archive was admitted to security mode: %#v", result)
	}

	securityPocket := origin
	securityPocket.Codename = "resolute-security"
	result = classifySuiteOptional(t, policy, suiteOptionalPackage("security-pocket", securityPocket), map[string]bool{"security-pocket": true}, nil)
	if !result.Valid || result.Counts.Eligible != 1 {
		t.Fatalf("security codename pocket with empty Archive was rejected: %#v", result)
	}
}

func TestAPTInventoryWithTailscaleReleaseWithoutSuite(t *testing.T) {
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
APT::Architectures { "amd64"; };
`, root, filepath.Join(root, "var/lib/dpkg/status")))
	t.Setenv("APT_CONFIG", filepath.Join(root, "apt.conf"))
	write("etc/apt/sources.list", "deb [trusted=yes] https://pkgs.tailscale.com/stable/debian bookworm main\n")
	write("var/lib/apt/lists/pkgs.tailscale.com_stable_debian_dists_bookworm_Release", "Origin: Tailscale\nLabel: Tailscale\nCodename: bookworm\nArchitectures: amd64\nComponents: main\n")
	write("var/lib/apt/lists/pkgs.tailscale.com_stable_debian_dists_bookworm_main_binary-amd64_Packages", "Package: tailscale\nArchitecture: amd64\nVersion: 2.0\nFilename: pool/tailscale_2.0_amd64.deb\nSize: 1\nDescription: Tailscale fixture\n\n")
	write("var/lib/dpkg/status", "Package: tailscale\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1.0\nDescription: Tailscale fixture\n\n")

	h := testHelper(t)
	h.Runner = ExecRunner{}
	inventory, err := h.readAPTInventory(ctx)
	if err != nil {
		t.Fatalf("shipped inventory failed with real python3-apt: %v", err)
	}
	if len(inventory.Packages) != 1 {
		t.Fatalf("got %d update candidates, want 1: %#v", len(inventory.Packages), inventory.Packages)
	}
	pkg := inventory.Packages[0]
	if pkg.Name != "tailscale" || pkg.Installed != "1.0" || pkg.Candidate != "2.0" || !pkg.NewerAvailable || pkg.CandidateNewer == nil || !*pkg.CandidateNewer {
		t.Fatalf("unexpected real APT candidate metadata: %#v", pkg)
	}
	if len(pkg.Origins) != 1 || pkg.Origins[0] != (inventoryOrigin{
		Origin: "Tailscale", Label: "Tailscale", Codename: "bookworm",
		Site: "pkgs.tailscale.com", Trusted: true,
	}) {
		t.Fatalf("Tailscale origin without Suite was not preserved: %#v", pkg.Origins)
	}
}
