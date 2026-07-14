package maintenance

import (
	"github.com/henrygd/beszel"
	entity "github.com/henrygd/beszel/internal/entities/maintenance"
)

const MinAgentVersion = "0.1.3"

// BuildCommit is populated by GoReleaser and remains descriptive in local builds.
var BuildCommit = "unknown"

func HelperVersion() string  { return beszel.PlusVersion }
func ProtocolVersion() uint8 { return entity.ProtocolVersion }
