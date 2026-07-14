package aptstatus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func createLockFixture(t *testing.T, paths []string, pid int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc", fmt.Sprint(pid)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", fmt.Sprint(pid), "comm"), []byte("unattended-upgrade\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", fmt.Sprint(pid), "cgroup"), []byte("0::/system.slice/apt-daily-upgrade.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var lines string
	for index, path := range paths {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(full)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		lines += fmt.Sprintf("%d: POSIX  ADVISORY  WRITE %d %x:%x:%d 0 EOF\n", index+1, pid, unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "locks"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInspectDeduplicatesHolderAcrossLocks(t *testing.T) {
	root := createLockFixture(t, []string{"var/lib/dpkg/lock", "var/lib/dpkg/lock-frontend"}, 48640)
	status, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Busy || len(status.Holders) != 1 || status.Holders[0].PID != 48640 || len(status.Holders[0].Locks) != 2 || status.Holders[0].Unit != "apt-daily-upgrade.service" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestProcessNameWithoutKernelLockIsNotBusy(t *testing.T) {
	root := createLockFixture(t, nil, 4242)
	status, err := Inspect(root)
	if err != nil || status.Busy {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestWaitObservesReleasedLock(t *testing.T) {
	root := createLockFixture(t, []string{"var/lib/dpkg/lock-frontend"}, 4242)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(root, "proc", "locks"), nil, 0o644)
	}()
	status, _, err := Wait(context.Background(), root, time.Second, 5*time.Millisecond, nil)
	if err != nil || status.Busy {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}
