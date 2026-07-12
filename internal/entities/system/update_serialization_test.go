package system

import (
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
	updateentity "github.com/henrygd/beszel/internal/entities/update"
)

func TestCombinedDataUpdateSerialization(t *testing.T) {
	without, err := json.Marshal(CombinedData{})
	if err != nil {
		t.Fatal(err)
	}
	if string(without) == "" {
		t.Fatal("empty JSON")
	}
	data := CombinedData{Updates: &updateentity.Status{Supported: true, OverallState: updateentity.OverallHealthy}}
	encoded, err := cbor.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CombinedData
	if err := cbor.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Updates == nil || decoded.Updates.OverallState != updateentity.OverallHealthy {
		t.Fatalf("CBOR update status lost: %#v", decoded.Updates)
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Updates == nil {
		t.Fatal("JSON update status lost")
	}
}
