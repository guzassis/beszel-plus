//go:build !linux

package agent

import (
	"context"

	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

func collectUpdateStatus(_ context.Context, _ updateOptions, _ *systemdManager) (*updateentity.Status, error) {
	return &updateentity.Status{
		Supported: false, InstallationState: updateentity.InstallationUnknown,
		ConfigurationState: updateentity.ConfigurationUnknown, TimerState: updateentity.TimerUnknown,
		ServiceState: updateentity.ServiceUnknown, LastResult: updateentity.ResultUnknown,
		OverallState: updateentity.OverallUnsupported,
	}, nil
}
