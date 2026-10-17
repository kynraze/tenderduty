package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainBridgeSetup(t *testing.T) {
	for _, mode := range []string{"token", "no-token", "rejected", "refused", "force", "missing-token", "keep-token", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			original = bytes.ReplaceAll(original, []byte("https://rpc.example"), []byte("http://127.0.0.1:1"))
			if err := os.WriteFile(filename, original, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			options.reload = func(string) (int, error) { calls++; return 123, nil }
			wantToken := mode == "token" || mode == "keep-token"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if wantToken && r.Header.Get("Authorization") != "Bearer test-token" {
					t.Errorf("missing authorization")
				}
				if !wantToken && r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected authorization")
				}
				if mode == "rejected" {
					w.WriteHeader(401)
					return
				}
				var request struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				switch request.Method {
				case "header.LocalHead":
					chain := "chain-1"
					if mode == "mismatch" {
						chain = "other"
					}
					json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{"header": map[string]string{"height": "1715094", "chain_id": chain}}})
				case "das.SamplingStats":
					w.Write([]byte(`{"result":{"head_of_sampled_chain":1715090}}`))
				default:
					t.Errorf("unexpected method %s", request.Method)
				}
			}))
			defer server.Close()
			address := server.URL
			if mode == "refused" || mode == "force" {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address = "http://" + listener.Addr().String()
				listener.Close()
			}
			args := []string{"bridge", "celestia", address}
			if mode == "token" {
				args = append(args, "--alert", "yes", "--endpoint", "http://metrics.example:4318")
			}
			tokenFile := filepath.Join(filepath.Dir(options.ConfigFile), "bridge.token")
			if wantToken {
				if err := os.WriteFile(tokenFile, []byte(" test-token\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "keep-token" {
					var err error
					original, err = applyChainSettings(original, []string{"bridge.token_file=" + tokenFile})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filename, original, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					args = append(args, "--token-file", tokenFile)
				}
			}
			if mode == "missing-token" {
				args = append(args, "--token-file", tokenFile)
			}
			if mode == "force" {
				args = append(args, "--force")
			}
			var output bytes.Buffer
			err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output)
			data, _ := os.ReadFile(filename)
			fail := mode == "rejected" || mode == "refused" || mode == "missing-token" || mode == "mismatch"
			if fail {
				if err == nil || !bytes.Equal(data, original) || calls != 0 {
					t.Fatalf("err %v, data %s, output %s, reloads %d", err, data, &output, calls)
				}
				if mode == "rejected" && !strings.Contains(output.String(), "token rejected (401)") {
					t.Fatalf("%s", &output)
				}
				if mode == "refused" && !strings.Contains(output.String(), "--rpc.addr 0.0.0.0") {
					t.Fatalf("%s", &output)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte(address)) || calls != 1 || !strings.Contains(output.String(), "--metrics.endpoint") {
				t.Fatalf("%s / %s / %d", data, &output, calls)
			}
			if mode != "force" && !strings.Contains(output.String(), "head 1,715,094 · sampled 1,715,090") {
				t.Fatalf("%s", &output)
			}
			if mode == "token" && (!bytes.Contains(data, []byte("bridge_enabled: yes")) || !strings.Contains(output.String(), "--metrics.endpoint metrics.example:4318")) {
				t.Fatalf("%s / %s", data, &output)
			}
			if mode == "no-token" && !strings.Contains(output.String(), "no token") {
				t.Fatalf("%s", &output)
			}
			if mode == "keep-token" && !bytes.Contains(data, []byte(tokenFile)) {
				t.Fatalf("token_file removed: %s", data)
			}
			if strings.Contains(output.String(), "test-token") {
				t.Fatalf("leaked token: %s", &output)
			}
		})
	}
}

func TestChainFibreSetup(t *testing.T) {
	for _, mode := range []string{"enable", "missing-listen", "missing-token", "competing", "instance", "off"} {
		t.Run(mode, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			config := "fibre_metrics:\n  listen: 127.0.0.1:4318\n  token: metrics-secret\n"
			if mode == "missing-listen" {
				config = "{}\n"
			}
			if mode == "missing-token" {
				config = "fibre_metrics:\n  listen: 10.0.0.1:4318\n"
			}
			if mode == "competing" {
				config += "alert_defaults:\n  fibre_enabled: yes\n"
				other := bytes.ReplaceAll(original, []byte("  fibre_enabled: false # Keep this comment\n"), nil)
				if err := os.WriteFile(filepath.Join(options.Directory, "Other.yml"), other, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(options.ConfigFile, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"fibre", "Celestia"}
			if mode == "instance" {
				args = append(args, "--instance", "fibre-host", "--endpoint", "http://metrics.example:4318")
			}
			if mode == "off" {
				args = append(args, "--off")
			}
			options.reload = func(string) (int, error) { return 123, nil }
			var output bytes.Buffer
			err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output)
			// the config was already missing the token; enabling Fibre doesn't add that error
			if mode == "missing-token" {
				if err != nil || !strings.Contains(output.String(), "Fibre metrics need a token") {
					t.Fatalf("%v / %s", err, &output)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filename)
			if mode == "off" {
				if !bytes.Contains(data, []byte("fibre_enabled: false")) || strings.Contains(output.String(), "fibre start") {
					t.Fatalf("%s / %s", data, &output)
				}
				return
			}
			if !bytes.Contains(data, []byte("fibre_enabled: true")) || !strings.Contains(output.String(), "fibre start --otel-endpoint") {
				t.Fatalf("%s / %s", data, &output)
			}
			if mode == "missing-listen" && !strings.Contains(output.String(), "Fibre metrics are off") {
				t.Fatalf("%s", &output)
			}
			// two unnamed chains are fine: the printed namespace routes each Fibre
			if mode == "competing" && strings.Contains(output.String(), "⚠") {
				t.Fatalf("%s", &output)
			}
			if mode == "instance" && (!bytes.Contains(data, []byte("fibre_instance: fibre-host")) || !strings.Contains(output.String(), "--otel-endpoint http://metrics.example:4318")) {
				t.Fatalf("%s", data)
			}
			if !strings.Contains(output.String(), `OTEL_RESOURCE_ATTRIBUTES="service.namespace=chain-1"`) {
				t.Fatalf("missing namespace: %s", &output)
			}
			if strings.Contains(output.String(), "metrics-secret") {
				t.Fatalf("secret leaked: %s", &output)
			}
		})
	}
}

func TestMetricsEndpoint(t *testing.T) {
	for _, tc := range []struct{ listen, remote, override, want string }{
		{"10.1.2.3:4318", "", "", "http://10.1.2.3:4318"},
		{"0.0.0.0:4318", "http://127.0.0.1:1234", "", "http://127.0.0.1:4318"},
		{"[::]:4318", "", "", "http://<this server's IP>:4318"},
		{"0.0.0.0:4318", "", "http://example:4318", "http://example:4318"},
	} {
		got, err := metricsEndpoint(FibreMetricsConfig{Listen: tc.listen}, tc.remote, tc.override)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %s, %v", tc, got, err)
		}
	}
}

func TestBridgeSetupWarnings(t *testing.T) {
	for _, tc := range []struct {
		name, codespace, threshold         string
		code                               uint32
		lag                                int64
		warnChain, warnLag, offline, force bool
	}{
		{name: "celestia", lag: 4},
		{name: "other-chain", codespace: "sdk", code: 6, lag: 4, warnChain: true},
		{name: "uncertain", codespace: "sdk", code: 2, lag: 4},
		{name: "unreachable", offline: true, lag: 4},
		{name: "at-threshold", lag: 20},
		{name: "behind", lag: 1234, warnLag: true},
		{name: "custom-threshold", threshold: "  bridge_lag_blocks: 2000\n", lag: 1234},
		{name: "forced-mismatch", codespace: "sdk", code: 6, lag: 4, warnChain: true, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			queries := 0
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     interface{} `json:"id"`
					Method string      `json:"method"`
					Params struct {
						Path string `json:"path"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				queries++
				if request.Method != "abci_query" || request.Params.Path != "/celestia.signal.v1.Query/VersionTally" {
					t.Errorf("unexpected query %+v", request)
				}
				response := map[string]interface{}{"code": tc.code, "codespace": tc.codespace, "log": "unknown request"}
				json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request.ID, "result": map[string]interface{}{"response": response}})
			}))
			defer node.Close()
			if tc.offline {
				node.Close()
			}
			original = bytes.ReplaceAll(original, []byte("https://rpc.example"), []byte(node.URL))
			if err := os.WriteFile(filename, original, 0600); err != nil {
				t.Fatal(err)
			}
			if tc.threshold != "" {
				if err := os.WriteFile(options.ConfigFile, []byte("alert_defaults:\n"+tc.threshold), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.Method == "header.LocalHead" {
					id := "chain-1"
					if tc.force {
						id = "other-1"
					}
					fmt.Fprintf(w, `{"result":{"header":{"height":"1715094","chain_id":%q}}}`, id)
				} else {
					fmt.Fprintf(w, `{"result":{"head_of_sampled_chain":%d}}`, 1715094-tc.lag)
				}
			}))
			defer bridge.Close()
			options.reload = func(string) (int, error) { return 123, nil }
			var output bytes.Buffer
			err := setupBridge(context.Background(), options, "Celestia", bridge.URL, "", "", "http://127.0.0.1:4318", tc.force, &output)
			if err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if strings.Contains(text, "doesn't look like Celestia") != tc.warnChain || strings.Contains(text, "sampling is ") != tc.warnLag {
				t.Fatal(text)
			}
			if tc.warnChain && strings.Count(text, "doesn't look like Celestia") != 1 {
				t.Fatal(text)
			}
			if tc.warnLag && !strings.Contains(text, "sampling is 1,234 blocks behind head;") {
				t.Fatal(text)
			}
			if !tc.offline && queries != 1 {
				t.Fatalf("queries %d", queries)
			}
			data, err := os.ReadFile(filename)
			if err != nil || !bytes.Contains(data, []byte(bridge.URL)) {
				t.Fatalf("not saved: %s, %v", data, err)
			}
		})
	}
}
