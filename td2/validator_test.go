package tenderduty

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cosmos/cosmos-sdk/types/bech32"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

func TestConsensusAddressStillMonitorsWhenSlashingQueryIsUnsupported(t *testing.T) {
	address, err := bech32.ConvertAndEncode("cosmosvalcons", make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID interface{} `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": request.ID,
			"error": map[string]interface{}{"code": -32601, "message": "slashing query unsupported"},
		})
	}))
	defer server.Close()
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	chain := &ChainConfig{ValAddress: address, client: client}
	if err := chain.GetValInfo(false); err == nil {
		t.Fatal("unsupported slashing query was reported as available")
	}
	info, _ := chain.validatorState()
	if info.Valcons != address || len(info.Conspub) != 20 || info.Window != 0 {
		t.Fatalf("block monitoring identity was lost: %+v", info)
	}
}

func TestCryptoOrgValconsPrefix(t *testing.T) {
	if prefix, ok := altValopers.getAltPrefix("crocncl1qz7k6tlc37u02yw95pp2rx2d"); !ok || prefix != "crocnclcons" {
		t.Fatalf("got %q %v", prefix, ok)
	}
}
