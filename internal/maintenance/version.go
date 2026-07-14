package maintenance

import (
	"github.com/henrygd/beszel/internal/buildinfo"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const MinAgentVersion = "0.2.0"

// BuildCommit is populated by GoReleaser and remains descriptive in local builds.
var BuildCommit = buildinfo.GitCommit

func HelperVersion() string  { return buildinfo.Version }
func ProtocolVersion() uint8 { return entity.ProtocolVersion }
