package power

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticsRoundTripPreservesWOLProbeDetails(t *testing.T) {
	original := &Diagnostics{
		Enabled:           true,
		State:             Ready,
		Reason:            "ready",
		SelectedInterface: "enp1s0",
		CollectedAt:       time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC),
		Interfaces: []InterfaceDiagnostic{{
			Interface:     "enp1s0",
			Type:          "ethernet",
			MAC:           "02:11:22:33:44:55",
			Physical:      true,
			Carrier:       true,
			WOLSupported:  true,
			WOLEnabled:    true,
			WOLProbePath:  "/usr/sbin/ethtool",
			WOLProbeError: "",
		}},
	}

	jsonData, err := json.Marshal(original)
	require.NoError(t, err)
	var fromJSON Diagnostics
	require.NoError(t, json.Unmarshal(jsonData, &fromJSON))
	require.Equal(t, original, &fromJSON)

	cborData, err := cbor.Marshal(original)
	require.NoError(t, err)
	var fromCBOR Diagnostics
	require.NoError(t, cbor.Unmarshal(cborData, &fromCBOR))
	require.Equal(t, original.Enabled, fromCBOR.Enabled)
	require.Equal(t, original.State, fromCBOR.State)
	require.Equal(t, original.SelectedInterface, fromCBOR.SelectedInterface)
	require.True(t, original.CollectedAt.Equal(fromCBOR.CollectedAt))
	require.Equal(t, original.Interfaces, fromCBOR.Interfaces)
}
