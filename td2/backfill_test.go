package tenderduty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackfillFillsUnobservedHeights(t *testing.T) {
	auditConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		height := r.URL.Query().Get("height")
		proposer, flag := "OTHER", 2
		switch height {
		case "97":
			flag = 1
		case "98":
			proposer = "ABCD"
		case "99":
			http.Error(w, "pruned", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"result":{"signed_header":{"header":{"proposer_address":%q},"commit":{"height":%q,"signatures":[{"validator_address":"ABCD","block_id_flag":%d}]}}}}`, proposer, height, flag)
	}))
	defer server.Close()

	// head 101, so places 0..4 are heights 100..96; 100 was seen live, 96 to 99 were not
	cc := &ChainConfig{ChainId: "test-1", lastBlockNum: 101, blocksResults: []int{3, -1, -1, -1, -1, 3}}
	cc.valInfo = &ValInfo{Bonded: true, Conspub: ToBytes("abcd")}
	if heights := cc.emptyHeights(); len(heights) != 4 || heights[0] != 99 || heights[3] != 96 {
		t.Fatalf("empty heights are %v", heights)
	}
	cc.backfill(context.Background(), server.URL, cc.emptyHeights())

	want := []int{3, -1, int(StatusProposed), int(Statusmissed), int(StatusSigned), 3}
	for i := range want {
		if cc.blocksResults[i] != want[i] {
			t.Fatalf("history is %v, want %v", cc.blocksResults, want)
		}
	}
}

func TestBackfillSkipsInactiveValidators(t *testing.T) {
	auditConfig(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	cc := &ChainConfig{lastBlockNum: 101, blocksResults: []int{-1, -1}}
	cc.valInfo = &ValInfo{Bonded: false, Conspub: ToBytes("abcd")}
	cc.backfill(context.Background(), server.URL, cc.emptyHeights())
	if requests != 0 || cc.blocksResults[0] != -1 {
		t.Fatal("an inactive validator's history was filled in")
	}
}
