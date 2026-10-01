package dash

import (
	"strings"
	"testing"
)

func TestHiddenLogsRedactEndpointDetails(t *testing.T) {
	status := &ChainStatus{LastError: "RPC https://private.example:26657 failed; tcp://backup.example:26657 is down", Blocks: []int{3, 0}}
	snapshot := statusSnapshot(status, true)
	if strings.Contains(snapshot.LastError, "private.example") || strings.Contains(snapshot.LastError, "backup.example") {
		t.Fatalf("private endpoint details were exposed: %s", snapshot.LastError)
	}
	if !strings.Contains(snapshot.LastError, "failed") || !strings.Contains(status.LastError, "private.example") {
		t.Fatal("redaction changed the original state or removed unrelated details")
	}
	snapshot.Blocks[0] = 0
	if status.Blocks[0] != 3 {
		t.Fatal("dashboard snapshot shares mutable block history")
	}
}
