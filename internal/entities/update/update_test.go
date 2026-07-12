package update

import "testing"

func u16(value uint16) *uint16 { return &value }

func TestDeriveOverallStatePrecedence(t *testing.T) {
	base := Status{Supported: true, InstallationState: InstallationInstalled, ConfigurationState: ConfigurationEnabled, TimerState: TimerActive, ServiceState: ServiceSuccess, LastResult: ResultSuccess, PendingUpdates: u16(0)}
	tests := []struct {
		name   string
		mutate func(*Status)
		want   OverallState
	}{
		{"unsupported", func(s *Status) { s.Supported = false }, OverallUnsupported},
		{"not installed", func(s *Status) { s.InstallationState = InstallationNotInstalled }, OverallNotInstalled},
		{"missing config", func(s *Status) { s.ConfigurationState = ConfigurationMissing }, OverallInstalledNotConfigured},
		{"partial config", func(s *Status) { s.ConfigurationState = ConfigurationPartial }, OverallInstalledNotConfigured},
		{"disabled", func(s *Status) { s.ConfigurationState = ConfigurationDisabled }, OverallConfiguredDisabled},
		{"failed before reboot", func(s *Status) { s.LastResult = ResultFailed; s.RebootRequired = true }, OverallLastRunFailed},
		{"running before reboot", func(s *Status) { s.LastResult = ResultRunning; s.RebootRequired = true }, OverallUpdateInProgress},
		{"reboot", func(s *Status) { s.RebootRequired = true }, OverallRebootRequired},
		{"pending", func(s *Status) { s.PendingUpdates = u16(5) }, OverallUpdatesPending},
		{"incomplete", func(s *Status) { s.ServiceState = ServiceUnknown }, OverallMonitoringIncomplete},
		{"healthy", func(*Status) {}, OverallHealthy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := base
			test.mutate(&status)
			if got := DeriveOverallState(&status); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
