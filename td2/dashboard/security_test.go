package dash

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServerTimeouts(t *testing.T) {
	server := newServer("0", http.NotFoundHandler())
	if server.ReadTimeout != 15*time.Second || server.IdleTimeout != 60*time.Second || server.ReadHeaderTimeout <= 0 {
		t.Fatalf("timeouts: read %v, idle %v, header %v", server.ReadTimeout, server.IdleTimeout, server.ReadHeaderTimeout)
	}
}

func TestWebsocketClientsAreLimited(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	handler := limitClients(2, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	})
	var running sync.WaitGroup
	for i := 0; i < 2; i++ {
		running.Add(1)
		go func() {
			defer running.Done()
			handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws", nil))
		}()
		<-entered
	}
	extra := httptest.NewRecorder()
	handler(extra, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if extra.Code != http.StatusServiceUnavailable {
		t.Fatalf("a client over the limit got %d, want 503", extra.Code)
	}
	close(release)
	running.Wait()
	again := httptest.NewRecorder()
	handler(again, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if again.Code != http.StatusOK {
		t.Fatalf("a slot was not freed: %d", again.Code)
	}
}

func fibreStatusWithDetails() *FibreStatus {
	return &FibreStatus{
		Registered: true, Host: "fibre.example:7980", Reachable: false, DownReason: "dial tcp 10.0.0.5:7980: connection refused",
		Identity: "failed", IdentityError: "key mismatch for fibre.internal:7980", Version: "v9.9.9-private",
		Resources: map[string]float64{"go_goroutines": 120, "go_memory_used_bytes": 1024, "go_processor_limit": 12},
	}
}

func TestHiddenLogsHideFibreDetails(t *testing.T) {
	original := &ChainStatus{Name: "Chain", Fibre: fibreStatusWithDetails()}
	hidden := statusSnapshot(original, true).Fibre
	if hidden.DownReason != "" || hidden.IdentityError != "" || hidden.Version == "" || hidden.Resources == nil {
		t.Fatalf("hidden status exposes details: %+v", hidden)
	}
	if hidden.Host != "fibre.example:7980" || hidden.Identity != "failed" || hidden.Reachable || !hidden.Registered {
		t.Fatalf("hidden status lost what is on chain or the state: %+v", hidden)
	}
	if original.Fibre.DownReason == "" || original.Fibre.Resources == nil {
		t.Fatal("hiding changed the original")
	}
	shown := statusSnapshot(original, false).Fibre
	if shown.DownReason == "" || shown.IdentityError == "" || shown.Version == "" || shown.Resources == nil {
		t.Fatalf("details were hidden with hide_logs off: %+v", shown)
	}
	if statusSnapshot(&ChainStatus{Name: "Chain"}, true).Fibre != nil {
		t.Fatal("a chain without Fibre got one")
	}
	data, err := json.Marshal(hidden)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"down_reason", "identity_error", "fibre.internal", "10.0.0.5"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("%s is in %s", leaked, data)
		}
	}
}

func TestHiddenLogsHideFibreHistoryDetails(t *testing.T) {
	provider := func(span, chain string) (FibreHistory, error) {
		return FibreHistory{Range: span, Chains: []FibreChainHistory{{Name: "Chain", Summary: fibreStatusWithDetails()}, {Name: "Bare"}}}, nil
	}
	for hide, wantDetails := range map[bool]bool{true: false, false: true} {
		wrapped := provider
		if hide {
			wrapped = hideFibreDetails(provider)
		}
		response := httptest.NewRecorder()
		FibreHandler(wrapped)(response, httptest.NewRequest(http.MethodGet, "/fibre", nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "fibre.example:7980") {
			t.Fatalf("hide=%v: %d %s", hide, response.Code, body)
		}
		for _, runtime := range []string{`"go_goroutines":120`, `"go_memory_used_bytes":1024`, `"go_processor_limit":12`} {
			if !strings.Contains(body, runtime) {
				t.Fatalf("runtime resources hidden: %s", body)
			}
		}
		for _, key := range []string{"down_reason", "identity_error"} {
			if strings.Contains(body, `"`+key+`"`) != wantDetails {
				t.Fatalf("hide=%v: %s present=%v in %s", hide, key, !wantDetails, body)
			}
		}
	}
}

func TestFibreNotificationBroadcast(t *testing.T) {
	var cast broadcaster
	messages, stop := cast.listen()
	defer stop()
	defer cast.close()
	message := fibreNotification(&ChainStatus{MsgType: "fibre", Name: "Chain", Fibre: &FibreStatus{MetricsAt: 123, DownReason: "private"}})
	cast.send(message)
	select {
	case body := <-messages:
		if string(body) != `{"msgType":"fibre","name":"Chain","metrics_at":123}` {
			t.Fatalf("notification: %s", body)
		}
	default:
		t.Fatal("export notification was not broadcast")
	}
	if fibreNotification(&ChainStatus{MsgType: "update"}) != nil {
		t.Fatal("normal status became a Fibre notification")
	}
}
