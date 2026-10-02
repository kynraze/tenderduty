package dash

import (
	"net/http"
	"net/http/httptest"
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

func TestHealthSeparatesLivenessAndReadiness(t *testing.T) {
	for _, state := range []HealthStatus{{Alive: true, Ready: false}, {Alive: true, Ready: true}, {Alive: false}} {
		for _, ready := range []bool{false, true} {
			response := httptest.NewRecorder()
			healthHandler(func() HealthStatus { return state }, ready)(response, httptest.NewRequest(http.MethodGet, "/", nil))
			expected := http.StatusOK
			if !state.Alive || (ready && !state.Ready) {
				expected = http.StatusServiceUnavailable
			}
			if response.Code != expected {
				t.Fatalf("state %+v readiness %v: got %d", state, ready, response.Code)
			}
		}
	}
}
