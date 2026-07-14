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
	if Version == "0.18.7" || Version == "0.18.2" {
		t.Fatal("product version silently fell back to upstream")
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
	dockerWorkflow, err := os.ReadFile(filepath.Join(root, ".github/workflows/docker-images.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dockerWorkflow), "tags:\n      - \"v*\"") || !strings.Contains(string(dockerWorkflow), "if: ${{ false }}") {
		t.Fatal("Docker publishing is not disabled for v0.2.0")
	}
}
