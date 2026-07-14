// Package beszel provides core application constants and version information
// which are used throughout the application.
package beszel

import (
	"github.com/blang/semver"
	"github.com/henrygd/beszel/internal/buildinfo"
)

const AppName = buildinfo.TechnicalAppName

// Version and PlusVersion remain aliases for source compatibility. New code
// should import internal/buildinfo directly.
var Version = buildinfo.Version
var PlusVersion = buildinfo.Version

// MinVersionCbor is the minimum supported version for CBOR compatibility.
var MinVersionCbor = semver.MustParse("0.12.0")

// MinVersionAgentResponse is the minimum supported version for AgentResponse compatibility.
var MinVersionAgentResponse = semver.MustParse("0.13.0")

// MinVersionMaintenance is the oldest Agent protocol version that may advertise
// typed operating-system maintenance capabilities. Capability negotiation is
// still required because upstream Agents can share this base version.
var MinVersionMaintenance = semver.MustParse("0.2.0-dev")
