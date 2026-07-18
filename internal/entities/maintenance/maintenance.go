// Package maintenance defines the typed update-management protocol shared by Hub, Agent and helper.
package maintenance

import (
	"errors"
	"regexp"
	"strings"
	"time"

	powerentity "github.com/henrygd/beszel/internal/entities/power"
)

const ProtocolVersion uint8 = 2

type Operation string
type OperationState string
type PolicyMode string

const (
	GetCapabilities           Operation = "get-capabilities"
	GetUpdatePolicy           Operation = "get-update-policy"
	DetectRepositories        Operation = "detect-repositories"
	ValidateUpdatePolicy      Operation = "validate-update-policy"
	ApplyUpdatePolicy         Operation = "apply-update-policy"
	InstallUpdateDependencies Operation = "install-update-dependencies"
	RunUpdateDryRun           Operation = "run-update-dry-run"
	RunUnattendedUpgrades     Operation = "run-unattended-upgrades"
	GetOperationStatus        Operation = "get-operation-status"
	GetPowerCapabilities      Operation = "get-power-capabilities"
	ProbeWOL                  Operation = "probe-wol"
	SchedulePoweroff          Operation = "schedule-poweroff"
	CancelPoweroff            Operation = "cancel-poweroff"
	GetPoweroffStatus         Operation = "get-poweroff-status"

	StateQueued    OperationState = "queued"
	StateRunning   OperationState = "running"
	StateCompleted OperationState = "completed"
	StateFailed    OperationState = "failed"

	ModeMonitorOnly PolicyMode = "monitor_only"
	ModeSecurity    PolicyMode = "security"
	ModeOfficialAll PolicyMode = "official_all"
	ModeCustom      PolicyMode = "custom"
)

var safeRepositoryValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:/-]{0,127}$`)
var safeInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

type Capabilities struct {
	UpdateMonitoring   bool                      `json:"update_monitoring" cbor:"0,keyasint"`
	UpdateManagement   bool                      `json:"update_management" cbor:"1,keyasint"`
	PrivilegedHelper   bool                      `json:"privileged_helper" cbor:"2,keyasint"`
	PolicyRead         bool                      `json:"policy_read" cbor:"3,keyasint"`
	PolicyWrite        bool                      `json:"policy_write" cbor:"4,keyasint"`
	DryRun             bool                      `json:"dry_run" cbor:"5,keyasint"`
	RunUpgrade         bool                      `json:"run_upgrade" cbor:"6,keyasint"`
	AutomaticReboot    bool                      `json:"automatic_reboot" cbor:"7,keyasint"`
	SupportedPlatform  string                    `json:"supported_platform,omitempty" cbor:"8,keyasint,omitempty"`
	HelperVersion      string                    `json:"helper_version,omitempty" cbor:"9,keyasint,omitempty"`
	ProtocolVersion    uint8                     `json:"protocol_version,omitempty" cbor:"10,keyasint,omitempty"`
	MinAgentVersion    string                    `json:"min_agent_version,omitempty" cbor:"11,keyasint,omitempty"`
	MaxProtocolVersion uint8                     `json:"max_protocol_version,omitempty" cbor:"12,keyasint,omitempty"`
	BuildCommit        string                    `json:"build_commit,omitempty" cbor:"13,keyasint,omitempty"`
	Power              *powerentity.Capabilities `json:"power,omitempty" cbor:"14,keyasint,omitempty"`
}

type Policy struct {
	Enabled                  bool       `json:"enabled" cbor:"0,keyasint"`
	Mode                     PolicyMode `json:"mode" cbor:"1,keyasint"`
	UpdatePackageListsDays   uint16     `json:"update_package_lists_days" cbor:"2,keyasint"`
	UnattendedUpgradeDays    uint16     `json:"unattended_upgrade_days" cbor:"3,keyasint"`
	AutomaticReboot          bool       `json:"automatic_reboot" cbor:"4,keyasint"`
	AutomaticRebootTime      string     `json:"automatic_reboot_time" cbor:"5,keyasint"`
	RemoveUnusedDependencies bool       `json:"remove_unused_dependencies" cbor:"6,keyasint"`
	AllowedRepositories      []string   `json:"allowed_repositories,omitempty" cbor:"7,keyasint,omitempty"`
	ConfirmThirdParty        bool       `json:"confirm_third_party,omitempty" cbor:"8,keyasint,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{Enabled: true, Mode: ModeSecurity, UpdatePackageListsDays: 1, UnattendedUpgradeDays: 1, AutomaticReboot: false, AutomaticRebootTime: "04:00"}
}

type Repository struct {
	ID            string   `json:"id" cbor:"0,keyasint"`
	Origin        string   `json:"origin" cbor:"1,keyasint"`
	Label         string   `json:"label" cbor:"2,keyasint"`
	Codename      string   `json:"codename" cbor:"3,keyasint"`
	Archive       string   `json:"archive" cbor:"4,keyasint"`
	Components    []string `json:"components,omitempty" cbor:"5,keyasint,omitempty"`
	Site          string   `json:"site,omitempty" cbor:"6,keyasint,omitempty"`
	Architectures []string `json:"architectures,omitempty" cbor:"7,keyasint,omitempty"`
	Trusted       bool     `json:"trusted" cbor:"8,keyasint"`
	Official      bool     `json:"official" cbor:"9,keyasint"`
}

type Request struct {
	Version        uint8     `json:"version" cbor:"0,keyasint"`
	RequestID      string    `json:"request_id" cbor:"1,keyasint"`
	Operation      Operation `json:"operation" cbor:"2,keyasint"`
	IdempotencyKey string    `json:"idempotency_key,omitempty" cbor:"3,keyasint,omitempty"`
	Policy         *Policy   `json:"policy,omitempty" cbor:"4,keyasint,omitempty"`
	DelaySeconds   uint32    `json:"delay_seconds,omitempty" cbor:"5,keyasint,omitempty"`
	Interfaces     []string  `json:"interfaces,omitempty" cbor:"6,keyasint,omitempty"`
}

type Result struct {
	Capabilities      *Capabilities                     `json:"capabilities,omitempty" cbor:"0,keyasint,omitempty"`
	Policy            *Policy                           `json:"policy,omitempty" cbor:"1,keyasint,omitempty"`
	Repositories      []Repository                      `json:"repositories,omitempty" cbor:"2,keyasint,omitempty"`
	Output            string                            `json:"output,omitempty" cbor:"3,keyasint,omitempty"`
	Changed           bool                              `json:"changed,omitempty" cbor:"4,keyasint,omitempty"`
	PowerCapabilities *powerentity.Capabilities         `json:"power_capabilities,omitempty" cbor:"5,keyasint,omitempty"`
	PoweroffStatus    *powerentity.ShutdownStatus       `json:"poweroff_status,omitempty" cbor:"6,keyasint,omitempty"`
	PowerInterfaces   []powerentity.InterfaceDiagnostic `json:"power_interfaces,omitempty" cbor:"7,keyasint,omitempty"`
}

type Response struct {
	Version        uint8          `json:"version" cbor:"0,keyasint"`
	RequestID      string         `json:"request_id" cbor:"1,keyasint"`
	Operation      Operation      `json:"operation" cbor:"2,keyasint"`
	Status         OperationState `json:"status" cbor:"3,keyasint"`
	Progress       uint8          `json:"progress,omitempty" cbor:"4,keyasint,omitempty"`
	Message        string         `json:"message,omitempty" cbor:"5,keyasint,omitempty"`
	Result         *Result        `json:"result,omitempty" cbor:"6,keyasint,omitempty"`
	Error          string         `json:"error,omitempty" cbor:"7,keyasint,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty" cbor:"8,keyasint,omitempty"`
	StartedAt      *time.Time     `json:"started_at,omitempty" cbor:"9,keyasint,omitempty"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty" cbor:"10,keyasint,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty" cbor:"11,keyasint,omitempty"`
	Stage          string         `json:"stage,omitempty" cbor:"12,keyasint,omitempty"`
	Retryable      bool           `json:"retryable,omitempty" cbor:"13,keyasint,omitempty"`
	Rollback       *Rollback      `json:"rollback,omitempty" cbor:"14,keyasint,omitempty"`
	LockFile       string         `json:"lock_file,omitempty" cbor:"15,keyasint,omitempty"`
	HolderPID      int            `json:"holder_pid,omitempty" cbor:"16,keyasint,omitempty"`
	HolderCommand  string         `json:"holder_command,omitempty" cbor:"17,keyasint,omitempty"`
	HolderUnit     string         `json:"holder_unit,omitempty" cbor:"18,keyasint,omitempty"`
	NextAttemptAt  *time.Time     `json:"next_attempt_at,omitempty" cbor:"19,keyasint,omitempty"`
}

type Rollback struct {
	Attempted bool `json:"attempted" cbor:"0,keyasint"`
	Succeeded bool `json:"succeeded" cbor:"1,keyasint"`
}

func IsOperationAllowed(op Operation) bool {
	switch op {
	case GetCapabilities, GetUpdatePolicy, DetectRepositories, ValidateUpdatePolicy, ApplyUpdatePolicy, InstallUpdateDependencies, RunUpdateDryRun, RunUnattendedUpgrades, GetOperationStatus, GetPowerCapabilities, ProbeWOL, SchedulePoweroff, CancelPoweroff, GetPoweroffStatus:
		return true
	default:
		return false
	}
}

func IsLongOperation(op Operation) bool {
	switch op {
	case ApplyUpdatePolicy, InstallUpdateDependencies, RunUpdateDryRun, RunUnattendedUpgrades, SchedulePoweroff:
		return true
	}
	return false
}

func ValidateRequest(req Request) error {
	if req.Version != ProtocolVersion {
		return errors.New("unsupported protocol version")
	}
	if !IsOperationAllowed(req.Operation) {
		return errors.New("operation not allowed")
	}
	if len(req.RequestID) < 8 || len(req.RequestID) > 64 || strings.ContainsAny(req.RequestID, "\r\n\x00") {
		return errors.New("invalid request ID")
	}
	if len(req.IdempotencyKey) > 128 || strings.ContainsAny(req.IdempotencyKey, "\r\n\x00") {
		return errors.New("invalid idempotency key")
	}
	if IsLongOperation(req.Operation) && len(req.IdempotencyKey) < 8 {
		return errors.New("idempotency key is required for long operations")
	}
	if req.Operation == ApplyUpdatePolicy || req.Operation == ValidateUpdatePolicy {
		if req.Policy == nil {
			return errors.New("policy is required")
		}
		return ValidatePolicy(*req.Policy)
	}
	if req.Policy != nil {
		return errors.New("policy is not valid for this operation")
	}
	if req.Operation == ProbeWOL {
		if len(req.Interfaces) == 0 || len(req.Interfaces) > 64 {
			return errors.New("at least one interface is required")
		}
		for _, name := range req.Interfaces {
			if !safeInterfaceName.MatchString(name) {
				return errors.New("invalid interface name")
			}
		}
	}
	if req.Operation != ProbeWOL && len(req.Interfaces) > 0 {
		return errors.New("interfaces are not valid for this operation")
	}
	if req.Operation == SchedulePoweroff && req.DelaySeconds > 604800 {
		return errors.New("poweroff delay out of range")
	}
	if req.Operation != SchedulePoweroff && req.DelaySeconds != 0 {
		return errors.New("delay is not valid for this operation")
	}
	return nil
}

func ValidatePolicy(policy Policy) error {
	switch policy.Mode {
	case ModeMonitorOnly, ModeSecurity, ModeOfficialAll, ModeCustom:
	default:
		return errors.New("invalid policy mode")
	}
	if policy.UpdatePackageListsDays > 365 || policy.UnattendedUpgradeDays > 365 {
		return errors.New("policy interval out of range")
	}
	if _, err := time.Parse("15:04", policy.AutomaticRebootTime); err != nil {
		return errors.New("invalid reboot time")
	}
	if len(policy.AllowedRepositories) > 64 {
		return errors.New("too many repositories")
	}
	for _, value := range policy.AllowedRepositories {
		if !safeRepositoryValue.MatchString(value) || strings.Contains(value, "..") {
			return errors.New("invalid repository identifier")
		}
	}
	if policy.Mode == ModeCustom && len(policy.AllowedRepositories) == 0 {
		return errors.New("custom policy requires repositories")
	}
	return nil
}
