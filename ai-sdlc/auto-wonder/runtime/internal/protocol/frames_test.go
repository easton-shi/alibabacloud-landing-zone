package protocol

import (
	"encoding/json"
	"testing"
)

func TestResultV2WireShape(t *testing.T) {
	encoded, err := json.Marshal(Result{
		Type: "TASK_RESULT", DispatchID: 42, ResultProtocolVersion: 2,
		ResultID: "8a3f4e3e-1111-4222-8333-444455556666", Success: true, ResultSummary: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["resultProtocolVersion"] != float64(2) || decoded["resultId"] == "" {
		t.Fatalf("missing V2 identity: %s", encoded)
	}
}

func TestHeartbeatCarriesInventory(t *testing.T) {
	encoded, _ := json.Marshal(Heartbeat{
		Type: "HEARTBEAT", ProtocolFeatures: []string{"dispatch_inventory_v1"},
		MaxConcurrentDispatches: 2, DispatchInventoryReady: true,
		RunningDispatchIDs: []int64{7}, OwnedDispatchIDs: []int64{7, 8},
		RunningConversationTurnIDs: []int64{},
	})
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	if decoded["dispatchInventoryReady"] != true || decoded["maxConcurrentDispatches"] != float64(2) {
		t.Fatalf("heartbeat shape: %s", encoded)
	}
}
