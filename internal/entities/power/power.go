// Package power defines backward-compatible power-management wire models.
package power

import "time"

type ReadinessState string

const (
	Ready              ReadinessState = "ready"
	Warning            ReadinessState = "warning"
	Disabled           ReadinessState = "disabled"
	Unsupported        ReadinessState = "unsupported"
	NoPhysicalEthernet ReadinessState = "no_physical_ethernet"
	NoCarrier          ReadinessState = "no_carrier"
	InvalidMAC         ReadinessState = "invalid_mac"
	WOLNotEnabled      ReadinessState = "wol_not_enabled"
	EthtoolMissing     ReadinessState = "ethtool_missing"
	NetworkUnassigned  ReadinessState = "network_unassigned"
	HubUnavailable     ReadinessState = "hub_unavailable"
	Stale              ReadinessState = "stale"
	Unknown            ReadinessState = "unknown"
)

type InterfaceDiagnostic struct {
	Interface    string `json:"interface" cbor:"0,keyasint"`
	Type         string `json:"type" cbor:"1,keyasint"`
	MAC          string `json:"mac,omitempty" cbor:"2,keyasint,omitempty"`
	IP           string `json:"ip,omitempty" cbor:"3,keyasint,omitempty"`
	Prefix       int    `json:"prefix,omitempty" cbor:"4,keyasint,omitempty"`
	Broadcast    string `json:"broadcast,omitempty" cbor:"5,keyasint,omitempty"`
	Driver       string `json:"driver,omitempty" cbor:"6,keyasint,omitempty"`
	OperState    string `json:"operstate,omitempty" cbor:"7,keyasint,omitempty"`
	Carrier      bool   `json:"carrier" cbor:"8,keyasint"`
	Speed        string `json:"speed,omitempty" cbor:"9,keyasint,omitempty"`
	Duplex       string `json:"duplex,omitempty" cbor:"10,keyasint,omitempty"`
	Physical     bool   `json:"physical" cbor:"11,keyasint"`
	WOLSupported bool   `json:"wol_supported" cbor:"12,keyasint"`
	WOLEnabled   bool   `json:"wol_enabled" cbor:"13,keyasint"`
}

type Diagnostics struct {
	Enabled     bool                  `json:"enabled" cbor:"0,keyasint"`
	State       ReadinessState        `json:"state" cbor:"1,keyasint"`
	Reason      string                `json:"reason,omitempty" cbor:"2,keyasint,omitempty"`
	Interfaces  []InterfaceDiagnostic `json:"interfaces,omitempty" cbor:"3,keyasint,omitempty"`
	CollectedAt time.Time             `json:"collected_at" cbor:"4,keyasint"`
}

type Capabilities struct {
	PowerManagement bool   `json:"power_management" cbor:"0,keyasint"`
	Shutdown        bool   `json:"shutdown" cbor:"1,keyasint"`
	CancelShutdown  bool   `json:"cancel_shutdown" cbor:"2,keyasint"`
	MaxDelaySeconds uint32 `json:"max_delay_seconds" cbor:"3,keyasint"`
}

type ShutdownStatus struct {
	Scheduled    bool       `json:"scheduled" cbor:"0,keyasint"`
	ScheduledAt  *time.Time `json:"scheduled_at,omitempty" cbor:"1,keyasint,omitempty"`
	DelaySeconds uint32     `json:"delay_seconds,omitempty" cbor:"2,keyasint,omitempty"`
}
