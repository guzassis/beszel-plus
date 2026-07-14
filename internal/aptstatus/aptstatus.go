// Package aptstatus inspects kernel locks held by APT and dpkg processes.
package aptstatus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var lockPaths = []string{
	"/var/lib/dpkg/lock",
	"/var/lib/dpkg/lock-frontend",
	"/var/lib/apt/lists/lock",
	"/var/cache/apt/archives/lock",
	"/run/unattended-upgrades.lock",
}

type Holder struct {
	PID     int      `json:"pid"`
	Command string   `json:"command,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Locks   []string `json:"locks"`
}

type Status struct {
	Busy    bool     `json:"busy"`
	Holders []Holder `json:"holders,omitempty"`
}

func Inspect(root string) (Status, error) {
	if root == "" {
		root = "/"
	}
	data, err := os.ReadFile(rootPath(root, "/proc/locks"))
	if err != nil {
		return Status{}, fmt.Errorf("read /proc/locks: %w", err)
	}
	type lockOwner struct{ pid int }
	owners := make(map[string]lockOwner)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		parts := strings.Split(fields[5], ":")
		if len(parts) != 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 {
			continue
		}
		owners[strings.ToLower(parts[0])+":"+strings.ToLower(parts[1])+":"+parts[2]] = lockOwner{pid: pid}
	}
	if err := scanner.Err(); err != nil {
		return Status{}, err
	}

	byPID := make(map[int]*Holder)
	for _, path := range lockPaths {
		info, err := os.Stat(rootPath(root, path))
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%x:%x:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)), stat.Ino)
		owner, ok := owners[key]
		if !ok {
			continue
		}
		holder := byPID[owner.pid]
		if holder == nil {
			holder = &Holder{PID: owner.pid, Command: readTrimmed(rootPath(root, fmt.Sprintf("/proc/%d/comm", owner.pid))), Unit: processUnit(root, owner.pid)}
			byPID[owner.pid] = holder
		}
		holder.Locks = append(holder.Locks, path)
	}

	status := Status{Busy: len(byPID) > 0, Holders: make([]Holder, 0, len(byPID))}
	for _, holder := range byPID {
		sort.Strings(holder.Locks)
		status.Holders = append(status.Holders, *holder)
	}
	sort.Slice(status.Holders, func(i, j int) bool { return status.Holders[i].PID < status.Holders[j].PID })
	return status, nil
}

func Wait(ctx context.Context, root string, timeout, interval time.Duration, observe func(Status)) (Status, time.Duration, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	started := time.Now()
	deadline := started.Add(timeout)
	for {
		status, err := Inspect(root)
		if err != nil {
			return status, time.Since(started), err
		}
		if observe != nil {
			observe(status)
		}
		if !status.Busy {
			return status, time.Since(started), nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return status, time.Since(started), nil
		}
		remaining := time.Until(deadline)
		waitFor := min(interval, remaining)
		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, time.Since(started), ctx.Err()
		case <-timer.C:
		}
	}
}

func rootPath(root, path string) string {
	if root == "" || root == "/" {
		return path
	}
	return filepath.Join(root, strings.TrimPrefix(path, "/"))
}

func readTrimmed(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

func processUnit(root string, pid int) string {
	data, err := os.ReadFile(rootPath(root, fmt.Sprintf("/proc/%d/cgroup", pid)))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, part := range strings.Split(line, "/") {
			part = strings.TrimSpace(part)
			if strings.HasSuffix(part, ".service") {
				return part
			}
		}
	}
	return ""
}

func IsContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
