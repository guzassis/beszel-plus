//go:build !linux

package agent

import (
	powerentity "github.com/henrygd/beszel/internal/entities/power"
	"time"
)

func collectPowerDiagnostics(enabled bool) *powerentity.Diagnostics {
	return &powerentity.Diagnostics{Enabled: enabled, State: powerentity.Unsupported, Reason: "power management is supported on Linux only", CollectedAt: time.Now().UTC()}
}
