// Package update contains the wire and domain model for automatic update monitoring.
package update

import "time"

type InstallationState string
type ConfigurationState string
type TimerState string
type ServiceState string
type LastResult string
type OverallState string

const (
	InstallationUnknown      InstallationState = "unknown"
	InstallationNotInstalled InstallationState = "not_installed"
	InstallationInstalled    InstallationState = "installed"

	ConfigurationUnknown  ConfigurationState = "unknown"
	ConfigurationMissing  ConfigurationState = "missing"
	ConfigurationPartial  ConfigurationState = "partial"
	ConfigurationDisabled ConfigurationState = "disabled"
	ConfigurationEnabled  ConfigurationState = "enabled"

	TimerUnknown  TimerState = "unknown"
	TimerNotFound TimerState = "not_found"
	TimerDisabled TimerState = "disabled"
	TimerInactive TimerState = "inactive"
	TimerActive   TimerState = "active"
	TimerFailed   TimerState = "failed"

	ServiceUnknown  ServiceState = "unknown"
	ServiceNotFound ServiceState = "not_found"
	ServiceInactive ServiceState = "inactive"
	ServiceRunning  ServiceState = "running"
	ServiceSuccess  ServiceState = "success"
	ServiceFailed   ServiceState = "failed"

	ResultUnknown  LastResult = "unknown"
	ResultNeverRun LastResult = "never_run"
	ResultRunning  LastResult = "running"
	ResultSuccess  LastResult = "success"
	ResultFailed   LastResult = "failed"

	OverallUnsupported            OverallState = "unsupported"
	OverallNotInstalled           OverallState = "not_installed"
	OverallInstalledNotConfigured OverallState = "installed_not_configured"
	OverallConfiguredDisabled     OverallState = "configured_disabled"
	OverallMonitoringIncomplete   OverallState = "monitoring_incomplete"
	OverallHealthy                OverallState = "healthy"
	OverallUpdatesPending         OverallState = "updates_pending"
	OverallRebootRequired         OverallState = "reboot_required"
	OverallLastRunFailed          OverallState = "last_run_failed"
	OverallUpdateInProgress       OverallState = "update_in_progress"
	OverallUnknown                OverallState = "unknown"
)

type UnitStatus struct {
	Name  string     `json:"name" cbor:"0,keyasint"`
	State TimerState `json:"state" cbor:"1,keyasint"`
	Next  *time.Time `json:"next,omitempty" cbor:"2,keyasint,omitempty"`
	Last  *time.Time `json:"last,omitempty" cbor:"3,keyasint,omitempty"`
}

// Status is a current snapshot. It is intentionally not part of minute-series data.
type Status struct {
	Supported               bool               `json:"supported" cbor:"0,keyasint"`
	PackageManager          string             `json:"package_manager,omitempty" cbor:"1,keyasint,omitempty"`
	InstallationState       InstallationState  `json:"installation_state" cbor:"2,keyasint"`
	ConfigurationState      ConfigurationState `json:"configuration_state" cbor:"3,keyasint"`
	TimerState              TimerState         `json:"timer_state" cbor:"4,keyasint"`
	ServiceState            ServiceState       `json:"service_state" cbor:"5,keyasint"`
	LastResult              LastResult         `json:"last_result" cbor:"6,keyasint"`
	OverallState            OverallState       `json:"overall_state" cbor:"7,keyasint"`
	PendingUpdates          *uint16            `json:"pending_updates,omitempty" cbor:"8,keyasint,omitempty"`
	PendingSecurityUpdates  *uint16            `json:"pending_security_updates,omitempty" cbor:"9,keyasint,omitempty"`
	LastCheckAt             *time.Time         `json:"last_check_at,omitempty" cbor:"10,keyasint,omitempty"`
	LastUpgradeAt           *time.Time         `json:"last_upgrade_at,omitempty" cbor:"11,keyasint,omitempty"`
	LastError               string             `json:"last_error,omitempty" cbor:"12,keyasint,omitempty"`
	RecentlyUpdatedPackages []string           `json:"recently_updated_packages,omitempty" cbor:"13,keyasint,omitempty"`
	RebootRequired          bool               `json:"reboot_required" cbor:"14,keyasint"`
	RebootRequiredBy        []string           `json:"reboot_required_by,omitempty" cbor:"15,keyasint,omitempty"`
	CollectedAt             time.Time          `json:"collected_at" cbor:"16,keyasint"`
	CacheAgeSeconds         uint32             `json:"cache_age_seconds" cbor:"17,keyasint"`
	DataSources             []string           `json:"data_sources,omitempty" cbor:"18,keyasint,omitempty"`
	Timers                  []UnitStatus       `json:"timers,omitempty" cbor:"19,keyasint,omitempty"`
	SecurityUpdatesSince    *time.Time         `json:"security_updates_since,omitempty" cbor:"20,keyasint,omitempty"`
	RebootRequiredSince     *time.Time         `json:"reboot_required_since,omitempty" cbor:"21,keyasint,omitempty"`
}

// DeriveOverallState is the single precedence definition used by agent and hub.
// Precedence: unsupported, not installed, not configured, disabled, failed,
// running, reboot, pending, incomplete, healthy, unknown.
func DeriveOverallState(s *Status) OverallState {
	if !s.Supported {
		return OverallUnsupported
	}
	if s.InstallationState == InstallationNotInstalled {
		return OverallNotInstalled
	}
	if s.InstallationState == InstallationInstalled && (s.ConfigurationState == ConfigurationMissing || s.ConfigurationState == ConfigurationPartial) {
		return OverallInstalledNotConfigured
	}
	if s.ConfigurationState == ConfigurationDisabled || s.TimerState == TimerDisabled {
		return OverallConfiguredDisabled
	}
	if s.LastResult == ResultFailed || s.ServiceState == ServiceFailed {
		return OverallLastRunFailed
	}
	if s.LastResult == ResultRunning || s.ServiceState == ServiceRunning {
		return OverallUpdateInProgress
	}
	if s.RebootRequired {
		return OverallRebootRequired
	}
	if s.PendingUpdates != nil && *s.PendingUpdates > 0 {
		return OverallUpdatesPending
	}
	if s.InstallationState == InstallationUnknown || s.ConfigurationState == ConfigurationUnknown || s.TimerState == TimerUnknown || s.ServiceState == ServiceUnknown || s.LastResult == ResultUnknown {
		return OverallMonitoringIncomplete
	}
	if s.InstallationState == InstallationInstalled && s.ConfigurationState == ConfigurationEnabled && s.TimerState == TimerActive {
		return OverallHealthy
	}
	return OverallUnknown
}
