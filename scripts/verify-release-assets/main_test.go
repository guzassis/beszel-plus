package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestVerifyAssetsChecksAllSixArchivesAndRejectsInvalidInputs(t *testing.T) {
	const version = "0.3.0"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name      string
		mutate    func(*testing.T, string)
		verify    func([]byte, assetSpec, string, string) error
		wantError string
	}{
		{name: "valid six assets", verify: verifyFixtureBinary},
		{
			name: "missing archive",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Remove(filepath.Join(dir, assetSpecs[0].archive)); err != nil {
					t.Fatal(err)
				}
			},
			verify:    verifyFixtureBinary,
			wantError: "missing archive",
		},
		{
			name: "corrupt archive checksum",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, assetSpecs[0].archive), []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify:    verifyFixtureBinary,
			wantError: "checksum mismatch",
		},
		{
			name: "wrong checksum manifest",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				path := filepath.Join(dir, "beszel_"+version+"_checksums.txt")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				lines := strings.Split(string(data), "\n")
				fields := strings.Fields(lines[0])
				if len(fields) != 2 {
					t.Fatalf("invalid generated fixture manifest line %q", lines[0])
				}
				lines[0] = strings.Repeat("0", 64) + "  " + fields[1]
				data = []byte(strings.Join(lines, "\n"))
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			verify:    verifyFixtureBinary,
			wantError: "checksum mismatch",
		},
		{
			name: "wrong component inside validly checksummed archive",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				spec := assetSpecs[0]
				data := fixtureMarker(assetSpecs[2], version, commit)
				if err := writeFixtureArchive(filepath.Join(dir, spec.archive), spec.binary, data); err != nil {
					t.Fatal(err)
				}
				rewriteFixtureManifest(t, dir, version)
			},
			verify:    verifyFixtureBinary,
			wantError: "wrong component",
		},
		{
			name: "wrong embedded version",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				spec := assetSpecs[0]
				if err := writeFixtureArchive(filepath.Join(dir, spec.archive), spec.binary, fixtureMarker(spec, "0.2.9", commit)); err != nil {
					t.Fatal(err)
				}
				rewriteFixtureManifest(t, dir, version)
			},
			verify:    verifyFixtureBinary,
			wantError: "wrong product version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			createFixtureAssets(t, dir, version, commit)
			if tt.mutate != nil {
				tt.mutate(t, dir)
			}
			err := verifyAssetsWith(dir, version, commit, tt.verify)
			if tt.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error=%v; want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestVerifyAssetsBindsSnapshotToCommit(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	if err := verifyAssetsWith(t.TempDir(), "0.3.0-snapshot."+commit, commit, verifyFixtureBinary); err == nil || !strings.Contains(err.Error(), "checksum manifest") {
		t.Fatalf("missing snapshot manifest should fail before inspection, got %v", err)
	}
	wrongCommit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := verifyAssetsWith(t.TempDir(), "0.3.0-snapshot."+wrongCommit, commit, verifyFixtureBinary); err == nil || !strings.Contains(err.Error(), "not the tagged target or this commit's snapshot") {
		t.Fatalf("snapshot with another commit should be rejected, got %v", err)
	}
}

func TestVerifyBuildMetadataBindsComponentTargetVersionAndCommit(t *testing.T) {
	const version = "0.3.0-snapshot.0123456789abcdef0123456789abcdef01234567"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	spec := assetSpecs[3]
	info := fixtureBuildInfo(spec, version, commit)
	if err := verifyBuildMetadata(info, spec, version, commit); err != nil {
		t.Fatal(err)
	}

	wrongComponent := *info
	wrongComponent.Path = assetSpecs[0].packagePath
	if err := verifyBuildMetadata(&wrongComponent, spec, version, commit); err == nil || !strings.Contains(err.Error(), "wrong component") {
		t.Fatalf("wrong component should fail, got %v", err)
	}
	wrongTarget := *info
	wrongTarget.Settings = append([]debug.BuildSetting(nil), info.Settings...)
	wrongTarget.Settings[1].Value = "amd64"
	if err := verifyBuildMetadata(&wrongTarget, spec, version, commit); err == nil || !strings.Contains(err.Error(), "wrong Go target") {
		t.Fatalf("wrong architecture should fail, got %v", err)
	}
	if err := verifyBuildMetadata(info, spec, "0.3.0", commit); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("wrong version should fail, got %v", err)
	}
	if err := verifyBuildMetadata(info, spec, version, strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("wrong commit should fail, got %v", err)
	}
	// A release must not accept a prerelease merely because its prefix matches.
	dev := fixtureBuildInfo(spec, "0.3.0-dev", commit)
	if err := verifyBuildMetadata(dev, spec, "0.3.0", commit); err == nil {
		t.Fatal("release accepted a dev binary")
	}
}

func TestArchiveBinaryPreservesReleaseDocumentationAndRejectsExtraEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		valid   bool
	}{
		{"release docs", []string{"LICENSE", "README.md", "beszel"}, true},
		{"extra binary", []string{"beszel", "beszel-agent"}, false},
		{"duplicate", []string{"beszel", "beszel"}, false},
		{"unsafe path", []string{"beszel", "../README.md"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer
			gz := gzip.NewWriter(&buffer)
			tarWriter := tar.NewWriter(gz)
			for _, name := range tc.entries {
				content := []byte(name)
				if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(content))}); err != nil {
					t.Fatal(err)
				}
				if _, err := tarWriter.Write(content); err != nil {
					t.Fatal(err)
				}
			}
			if err := tarWriter.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := archiveBinary(buffer.Bytes(), "beszel")
			if (err == nil) != tc.valid || (tc.valid && string(data) != "beszel") {
				t.Fatalf("archive result=%q err=%v", data, err)
			}
		})
	}
}

func fixtureBuildInfo(spec assetSpec, version, commit string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Path: spec.packagePath,
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"},
			{Key: "GOARCH", Value: spec.arch},
			{Key: "-ldflags", Value: "-X github.com/henrygd/beszel/internal/buildinfo.Version=" + version + " -X github.com/henrygd/beszel/internal/buildinfo.GitCommit=" + commit},
			{Key: "vcs.revision", Value: commit},
		},
	}
}

func createFixtureAssets(t *testing.T, dir, version, commit string) {
	t.Helper()
	for _, spec := range assetSpecs {
		if err := writeFixtureArchive(filepath.Join(dir, spec.archive), spec.binary, fixtureMarker(spec, version, commit)); err != nil {
			t.Fatal(err)
		}
	}
	rewriteFixtureManifest(t, dir, version)
}

func fixtureMarker(spec assetSpec, version, commit string) []byte {
	return []byte(fmt.Sprintf("package=%s\nversion=%s\ncommit=%s\n", spec.packagePath, version, commit))
}

func writeFixtureArchive(path, binaryName string, binary []byte) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	err = writer.WriteHeader(&tar.Header{Name: binaryName, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	if err == nil {
		_, err = writer.Write(binary)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func rewriteFixtureManifest(t *testing.T, dir, version string) {
	t.Helper()
	var lines []string
	for _, spec := range assetSpecs {
		data, err := os.ReadFile(filepath.Join(dir, spec.archive))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf("%x  %s", hash, spec.archive))
	}
	manifest := "beszel_" + version + "_checksums.txt"
	if err := os.WriteFile(filepath.Join(dir, manifest), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func verifyFixtureBinary(data []byte, spec assetSpec, version, commit string) error {
	if !bytes.Contains(data, []byte("package="+spec.packagePath)) {
		return fmt.Errorf("wrong component")
	}
	if !bytes.Contains(data, []byte("version="+version)) {
		return fmt.Errorf("wrong product version")
	}
	if !bytes.Contains(data, []byte("commit="+commit)) {
		return fmt.Errorf("wrong commit")
	}
	return nil
}
