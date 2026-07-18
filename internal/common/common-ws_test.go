package common

import (
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

func TestDataRequestOptionsRoundTripForcePowerDiagnostics(t *testing.T) {
	original := DataRequestOptions{CacheTimeMs: 60_000, ForcePowerDiagnostics: true}
	data, err := cbor.Marshal(original)
	require.NoError(t, err)
	var decoded DataRequestOptions
	require.NoError(t, cbor.Unmarshal(data, &decoded))
	require.Equal(t, original, decoded)
}
