package dash

import "testing"

func TestRPCNodesSnapshotPrivacy(t *testing.T) {
	original := &ChainStatus{RPCNodes: []RPCNodeStatus{{Endpoint: "localhost:26657", Version: "v10.4.0"}}}
	if got := statusSnapshot(original, true); len(got.RPCNodes) != 0 {
		t.Fatalf("hidden snapshot exposes nodes: %+v", got.RPCNodes)
	}
	visible := statusSnapshot(original, false)
	if len(visible.RPCNodes) != 1 || visible.RPCNodes[0].Version != "v10.4.0" {
		t.Fatalf("visible snapshot lost version: %+v", visible.RPCNodes)
	}
	visible.RPCNodes[0].Version = "v11.0.0"
	if original.RPCNodes[0].Version != "v10.4.0" {
		t.Fatal("snapshot shares mutable node list")
	}
}
