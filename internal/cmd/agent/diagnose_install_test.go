package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func diagnosticRoot(t *testing.T, busy bool) string {
	t.Helper()
	root := t.TempDir()
	pid := 48640
	for _, dir := range []string{"proc", fmt.Sprintf("proc/%d", pid), "var/lib/dpkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("proc/%d/comm", pid)), []byte("unattended-upgrade\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("proc/%d/cgroup", pid)), []byte("0::/system.slice/apt-daily-upgrade.service\n"), 0o644)
	lock := filepath.Join(root, "var/lib/dpkg/lock-frontend")
	_ = os.WriteFile(lock, nil, 0o600)
	contents := ""
	if busy {
		info, _ := os.Stat(lock)
		stat := info.Sys().(*syscall.Stat_t)
		contents = fmt.Sprintf("1: POSIX ADVISORY WRITE %d %x:%x:%d 0 EOF\n", pid, unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
	}
	_ = os.WriteFile(filepath.Join(root, "proc/locks"), []byte(contents), 0o644)
	return root
}

func TestDiagnoseInstallAllowsUnrelatedAPTActivity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := runDiagnoseInstall([]string{"--apt-required=false", "--wait-for-apt=60", "--json"}, &stdout, &stderr, diagnosticRoot(t, true), time.Millisecond)
	if status != 0 {
		t.Fatalf("status=%d stderr=%s", status, stderr.String())
	}
	var report installDiagnostic
	if json.Unmarshal(stdout.Bytes(), &report) != nil || !report.CanProceed || !report.APT.Busy || len(report.APT.Holders) != 1 {
		t.Fatalf("report=%s", stdout.String())
	}
}

func TestDiagnoseInstallBlocksRequiredAPTActivity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := runDiagnoseInstall([]string{"--apt-required=true", "--wait-for-apt=0", "--json"}, &stdout, &stderr, diagnosticRoot(t, true), time.Millisecond)
	if status != 75 {
		t.Fatalf("status=%d stdout=%s stderr=%s", status, stdout.String(), stderr.String())
	}
}

func TestDiagnoseInstallRejectsInvalidWait(t *testing.T) {
	status := runDiagnoseInstall([]string{"--wait-for-apt=3601"}, &bytes.Buffer{}, &bytes.Buffer{}, diagnosticRoot(t, false), time.Millisecond)
	if status != 64 {
		t.Fatalf("status=%d", status)
	}
}
