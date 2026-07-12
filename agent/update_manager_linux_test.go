//go:build linux

package agent

import (
	"testing"
	"time"
)

func TestParseAptConfig(t *testing.T) {
	values := parseAptConfig([]byte("// ignored\nAPT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"0\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"))
	if values["APT::Periodic::Update-Package-Lists"] != "1" || values["APT::Periodic::Unattended-Upgrade"] != "1" {
		t.Fatalf("unexpected values: %#v", values)
	}
	if !aptConfigEnabled("1") || aptConfigEnabled("0") || !aptConfigEnabled("true") {
		t.Fatal("enabled parsing failed")
	}
}

func TestParsePendingUpdates(t *testing.T) {
	total, security, ok := parseAptCheck([]byte("5;2\n"))
	if !ok || total != 5 || security != 2 {
		t.Fatalf("got %d;%d, %v", total, security, ok)
	}
	if _, _, ok = parseAptCheck([]byte("2;5")); ok {
		t.Fatal("accepted security count greater than total")
	}
	count, ok := parseAptSimulation([]byte("Inst curl [1] (2 repo)\nInst openssl [1] (2 repo)\n"))
	if !ok || count != 2 {
		t.Fatalf("simulation count = %d, %v", count, ok)
	}
}

func TestParseLatestAptHistoryLimitsAndDeduplicates(t *testing.T) {
	data := []byte("Start-Date: 2026-07-10  04:18:00\nUpgrade: old:amd64 (1, 2)\n\nStart-Date: 2026-07-11  04:18:00\nUpgrade: curl:amd64 (1, 2), openssl:amd64 (1, 2), curl:amd64 (1, 2)\nEnd-Date: 2026-07-11  04:18:02\n")
	when, packages := parseLatestAptHistory(data, 2)
	if when == nil || when.Year() != 2026 || len(packages) != 2 || packages[0] != "curl" || packages[1] != "openssl" {
		t.Fatalf("got %v %#v", when, packages)
	}
}

func TestUpdateSnapshotCopiesCache(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Second)
	m := &updateManager{status: nil, lastAttempt: now}
	if got := m.snapshot(); got == nil || got.OverallState != "monitoring_incomplete" {
		t.Fatalf("unexpected initial snapshot: %#v", got)
	}
}
