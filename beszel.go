// Package beszel provides core application constants and version information
// which are used throughout the application.
package beszel

import "github.com/blang/semver"

const (
	// Version is the current version of the application.
	Version = "0.18.7"
	// PlusVersion versions Beszel Plus release components independently from
	// the upstream-compatible protocol version above.
	PlusVersion = "0.1.3"
	// AppName is the name of the application.
	AppName = "beszel"
)

// MinVersionCbor is the minimum supported version for CBOR compatibility.
var MinVersionCbor = semver.MustParse("0.12.0")

// MinVersionAgentResponse is the minimum supported version for AgentResponse compatibility.
var MinVersionAgentResponse = semver.MustParse("0.13.0")

// MinVersionMaintenance is the oldest Agent protocol version that may advertise
// typed operating-system maintenance capabilities. Capability negotiation is
// still required because upstream Agents can share this base version.
var MinVersionMaintenance = semver.MustParse("0.18.7")
