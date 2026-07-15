package alerts

import (
	"testing"
	"time"

	"github.com/henrygd/beszel/internal/entities/system"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

func TestEvaluateUpdateAlert(t *testing.T) {
	security := uint16(2)
	now := time.Now().UTC()
	securitySince := now.Add(-49 * time.Hour)
	rebootSince := now.Add(-8 * 24 * time.Hour)
	collectedAt := now.Add(-25 * time.Hour)
	data := &system.CombinedData{Updates: &updateentity.Status{Supported: true, InstallationState: updateentity.InstallationNotInstalled, PendingSecurityUpdates: &security, SecurityUpdatesSince: &securitySince, RebootRequired: true, RebootRequiredSince: &rebootSince, CollectedAt: &collectedAt}}
	tests := []struct {
		name      string
		threshold float64
		want      bool
	}{
		{"Unattended upgrades package missing", 0, true},
		{"Security updates pending", 48, true},
		{"Reboot required", 7, true},
		{"Update information stale", 24, true},
	}
	for _, test := range tests {
		triggered, known, _, _ := evaluateUpdateAlert(test.name, test.threshold, data, now)
		if !known || triggered != test.want {
			t.Fatalf("%s: triggered=%v known=%v", test.name, triggered, known)
		}
	}
}

func TestEvaluateUpdateAlertIgnoresUnknown(t *testing.T) {
	data := &system.CombinedData{Updates: &updateentity.Status{Supported: true, InstallationState: updateentity.InstallationUnknown}}
	if _, known, _, _ := evaluateUpdateAlert("Unattended upgrades package missing", 0, data, time.Now()); known {
		t.Fatal("unknown installation state must not trigger or resolve an alert")
	}
	if _, known, _, _ := evaluateUpdateAlert("Security updates pending", 48, data, time.Now()); known {
		t.Fatal("unknown security count must not trigger or resolve an alert")
	}
}
