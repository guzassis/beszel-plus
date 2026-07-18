package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductMetadata(t *testing.T) {
	if ProductName != "Beszel Plus" || ProductSlug != "beszel-plus" {
		t.Fatal("unexpected product identity")
	}
	if RepositoryOwner != "guzassis" || RepositoryName != "beszel-plus" {
		t.Fatal("unexpected repository identity")
	}
	if UpstreamVersion != "0.18.2" {
		t.Fatalf("unexpected upstream compatibility version %q", UpstreamVersion)
	}
	if Version != "0.2.9-dev" {
		t.Fatalf("unexpected development product version %q", Version)
	}
}

func TestVersionFallbacksAndInstallersStayAligned(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, check := range []struct{ path, marker string }{
		{"internal/site/src/lib/build-info.ts", `"0.2.9-dev"`},
		{"supplemental/scripts/install-agent.sh", `PRODUCT_VERSION="0.2.9"`},
		{"supplemental/scripts/install-hub.sh", `PRODUCT_VERSION="0.2.9"`},
	} {
		data, err := os.ReadFile(filepath.Join(root, check.path))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), check.marker) {
			t.Errorf("%s is not aligned with v0.2.9", check.path)
		}
	}
}

func TestVisibleBrandingDoesNotPointAtUpstreamRepository(t *testing.T) {
	root := filepath.Join("..", "..")
	paths := []string{
		"internal/site/src/components/footer-repo-link.tsx",
		"internal/site/src/lib/build-info.ts",
		"supplemental/scripts/install-agent.sh",
		"supplemental/scripts/install-hub.sh",
	}
	for _, name := range paths {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "github.com/henrygd/beszel") {
			t.Errorf("visible upstream repository reference in %s", name)
		}
	}
}

func TestGeneratedLinuxInstallCommandExposesManagementChoices(t *testing.T) {
	root := filepath.Join("..", "..")
	commandSource, err := os.ReadFile(filepath.Join(root, "internal/site/src/components/install-dropdowns.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(commandSource)
	for _, flag := range []string{"--agent-auto-update=${options.agentAutoUpdate}", "--os-update-management=${options.osUpdateManagement}", "--os-update-policy=${options.osUpdatePolicy}", "--power-management=${options.powerManagement}", "--wait-for-apt=60"} {
		if !strings.Contains(text, flag) {
			t.Errorf("generated command is missing %q", flag)
		}
	}
	controls, err := os.ReadFile(filepath.Join(root, "internal/site/src/components/add-system.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"osUpdateManagement", "osUpdatePolicy", "powerManagement", "agentAutoUpdate"} {
		if !strings.Contains(string(controls), option) {
			t.Errorf("install UI is missing %q control", option)
		}
	}
}

func TestReleaseMatrixIsNativeLinuxOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	config, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(config)
	for _, forbidden := range []string{"darwin", "windows", "freebsd", "mips", "riscv", "nfpms:"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("unsupported release target %q remains", forbidden)
		}
	}
	if strings.Count(text, "goos: [linux]") != 3 || strings.Count(text, "goarch: [amd64, arm64]") != 3 {
		t.Fatal("Hub, Agent and helper must all target linux/amd64 and linux/arm64")
	}
	for _, mainPackage := range []string{
		"main: ./internal/cmd/hub",
		"main: ./internal/cmd/agent",
		"main: ./internal/cmd/maintenance-helper",
	} {
		if !strings.Contains(text, mainPackage) {
			t.Errorf("GoReleaser is missing complete command package %q", mainPackage)
		}
	}
	for _, archiveID := range []string{"id: hub-archive", "id: agent-archive", "id: helper-archive"} {
		if !strings.Contains(text, archiveID) {
			t.Errorf("GoReleaser archive is missing distinct ID %q", archiveID)
		}
	}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "main:") && strings.HasSuffix(line, ".go") {
			t.Errorf("GoReleaser entrypoint must be a package, not a single file: %s", line)
		}
	}
	releaseWorkflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(releaseWorkflow)
	for _, marker := range []string{
		"actions/checkout@v6",
		"goreleaser/goreleaser-action@v7",
		"version: v2.17.0",
		"release --snapshot --clean",
		"beszel-maintenance-helper_linux_arm64.tar.gz",
	} {
		if !strings.Contains(workflow, marker) {
			t.Errorf("release workflow is missing %q", marker)
		}
	}
	dockerWorkflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/docker-images.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dockerWorkflow), "tags:\n      - \"v*\"") || !strings.Contains(string(dockerWorkflow), "if: ${{ false }}") {
		t.Fatal("Docker publishing is not disabled for v0.2.0")
	}
}
