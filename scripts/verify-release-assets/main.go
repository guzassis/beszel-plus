package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	gobuildinfo "debug/buildinfo"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxArchiveBytes = 256 << 20

type assetSpec struct {
	archive     string
	binary      string
	component   string
	packagePath string
	arch        string
	machine     elf.Machine
}

var assetSpecs = []assetSpec{
	{archive: "beszel_linux_amd64.tar.gz", binary: "beszel", component: "Hub", packagePath: "github.com/henrygd/beszel/internal/cmd/hub", arch: "amd64", machine: elf.EM_X86_64},
	{archive: "beszel_linux_arm64.tar.gz", binary: "beszel", component: "Hub", packagePath: "github.com/henrygd/beszel/internal/cmd/hub", arch: "arm64", machine: elf.EM_AARCH64},
	{archive: "beszel-agent_linux_amd64.tar.gz", binary: "beszel-agent", component: "Agent", packagePath: "github.com/henrygd/beszel/internal/cmd/agent", arch: "amd64", machine: elf.EM_X86_64},
	{archive: "beszel-agent_linux_arm64.tar.gz", binary: "beszel-agent", component: "Agent", packagePath: "github.com/henrygd/beszel/internal/cmd/agent", arch: "arm64", machine: elf.EM_AARCH64},
	{archive: "beszel-maintenance-helper_linux_amd64.tar.gz", binary: "beszel-maintenance-helper", component: "Maintenance Helper", packagePath: "github.com/henrygd/beszel/internal/cmd/maintenance-helper", arch: "amd64", machine: elf.EM_X86_64},
	{archive: "beszel-maintenance-helper_linux_arm64.tar.gz", binary: "beszel-maintenance-helper", component: "Maintenance Helper", packagePath: "github.com/henrygd/beszel/internal/cmd/maintenance-helper", arch: "arm64", machine: elf.EM_AARCH64},
}

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: verify-release-assets <dist-dir> <version> <commit>")
		os.Exit(64)
	}
	if err := verifyAssets(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "release asset verification failed:", err)
		os.Exit(1)
	}
	fmt.Printf("verified six Linux Hub/Agent/helper archives and checksums for %s (%s)\n", os.Args[2], os.Args[3])
}

func verifyAssets(distDir, version, commit string) error {
	return verifyAssetsWith(distDir, version, commit, verifyBinary)
}

func verifyAssetsWith(distDir, version, commit string, verify func([]byte, assetSpec, string, string) error) error {
	if !fullCommitPattern.MatchString(commit) {
		return fmt.Errorf("commit must be a full lowercase Git SHA: %q", commit)
	}
	baseVersion, suffix, snapshot := strings.Cut(version, "-snapshot.")
	if !releaseVersionPattern.MatchString(baseVersion) || (snapshot && suffix != commit) {
		return fmt.Errorf("version %q is not the tagged target or this commit's snapshot", version)
	}
	manifestName := "beszel_" + version + "_checksums.txt"
	manifestPath := filepath.Join(distDir, manifestName)
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read checksum manifest %s: %w", manifestName, err)
	}
	checksums, err := parseChecksumManifest(manifest)
	if err != nil {
		return err
	}
	if len(checksums) != len(assetSpecs) {
		return fmt.Errorf("checksum manifest has %d entries; want exactly %d", len(checksums), len(assetSpecs))
	}

	for _, spec := range assetSpecs {
		wantHash, ok := checksums[spec.archive]
		if !ok {
			return fmt.Errorf("checksum manifest is missing %s", spec.archive)
		}
		archivePath := filepath.Join(distDir, spec.archive)
		info, err := os.Lstat(archivePath)
		if err != nil {
			return fmt.Errorf("missing archive %s: %w", spec.archive, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("archive %s is not a regular file", spec.archive)
		}
		if info.Size() <= 0 || info.Size() > maxArchiveBytes {
			return fmt.Errorf("archive %s has invalid size %d", spec.archive, info.Size())
		}
		archiveBytes, err := os.ReadFile(archivePath)
		if err != nil {
			return fmt.Errorf("read archive %s: %w", spec.archive, err)
		}
		hash := sha256.Sum256(archiveBytes)
		if fmt.Sprintf("%x", hash) != wantHash {
			return fmt.Errorf("checksum mismatch for %s", spec.archive)
		}
		binaryData, err := archiveBinary(archiveBytes, spec.binary)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", spec.archive, err)
		}
		if err := verify(binaryData, spec, version, commit); err != nil {
			return fmt.Errorf("%s: %w", spec.archive, err)
		}
	}
	return nil
}

func parseChecksumManifest(data []byte) (map[string]string, error) {
	checksums := make(map[string]string, len(assetSpecs))
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid checksum manifest line %d", lineNumber+1)
		}
		for _, char := range fields[0] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
				return nil, fmt.Errorf("invalid checksum on line %d", lineNumber+1)
			}
		}
		name := strings.TrimLeft(fields[1], "*")
		if filepath.Base(name) != name || name == "." || name == ".." {
			return nil, fmt.Errorf("unsafe asset path in checksum manifest line %d", lineNumber+1)
		}
		if _, duplicate := checksums[name]; duplicate {
			return nil, fmt.Errorf("duplicate checksum entry for %s", name)
		}
		checksums[name] = strings.ToLower(fields[0])
	}
	if len(checksums) == 0 {
		return nil, errors.New("checksum manifest is empty")
	}
	return checksums, nil
}

func archiveBinary(data []byte, expectedName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var binaryData []byte
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unexpected tar entry %q", header.Name)
		}
		seen[header.Name] = true
		if header.Name != expectedName {
			// Preserve GoReleaser's default license/readme files while rejecting
			// extra executables, paths, links, and unbounded documentation.
			switch header.Name {
			case "LICENSE", "LICENSE.md", "LICENSE.txt", "README.md", "CHANGELOG.md":
				if header.Size < 0 || header.Size > 1<<20 {
					return nil, fmt.Errorf("invalid documentation size: %q", header.Name)
				}
				continue
			default:
				return nil, fmt.Errorf("unexpected tar entry %q", header.Name)
			}
		}
		if header.Size <= 0 || header.Size > maxArchiveBytes {
			return nil, fmt.Errorf("binary has invalid size %d", header.Size)
		}
		binaryData, err = io.ReadAll(io.LimitReader(reader, maxArchiveBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read binary: %w", err)
		}
		if int64(len(binaryData)) != header.Size || len(binaryData) > maxArchiveBytes {
			return nil, fmt.Errorf("binary size does not match tar header")
		}
	}
	if len(binaryData) == 0 {
		return nil, errors.New("archive must contain the expected binary")
	}
	return binaryData, nil
}

func verifyBinary(data []byte, spec assetSpec, version, commit string) error {
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid ELF binary: %w", err)
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || file.Machine != spec.machine {
		return fmt.Errorf("wrong ELF target: class=%s data=%s machine=%s", file.Class, file.Data, file.Machine)
	}
	info, err := gobuildinfo.Read(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("read Go build metadata: %w", err)
	}
	return verifyBuildMetadata(info, spec, version, commit)
}

func verifyBuildMetadata(info *gobuildinfo.BuildInfo, spec assetSpec, version, commit string) error {
	if info.Path != spec.packagePath {
		return fmt.Errorf("wrong component package %q; want %q", info.Path, spec.packagePath)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != spec.arch {
		return fmt.Errorf("wrong Go target %s/%s; want linux/%s", settings["GOOS"], settings["GOARCH"], spec.arch)
	}
	ldflags := settings["-ldflags"]
	if linkerValue(ldflags, "github.com/henrygd/beszel/internal/buildinfo.Version") != version {
		return fmt.Errorf("embedded product version does not match %s", version)
	}
	if linkerValue(ldflags, "github.com/henrygd/beszel/internal/buildinfo.GitCommit") != commit {
		return fmt.Errorf("embedded commit does not match %s", commit)
	}
	if revision := settings["vcs.revision"]; revision != "" && revision != commit {
		return fmt.Errorf("Go VCS revision %s does not match %s", revision, commit)
	}
	return nil
}

func linkerValue(flags, key string) string {
	value := ""
	for _, flag := range strings.Fields(flags) {
		if candidate, ok := strings.CutPrefix(strings.Trim(flag, "\"'"), key+"="); ok {
			value = candidate
		}
	}
	return value
}
