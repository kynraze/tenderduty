package tenderduty

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVotingPowerAcrossPages(t *testing.T) {
	pages := map[string]string{
		"1": `{"result":{"validators":[{"address":"AAAA","voting_power":"500","pub_key":{"type":"tendermint/PubKeyBn254","value":"x"}},{"address":"ABCD","voting_power":"300"}],"total":"3"}}`,
		"2": `{"result":{"validators":[{"address":"CCCC","voting_power":200}],"total":3}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validators" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = fmt.Fprint(w, pages[r.URL.Query().Get("page")])
	}))
	defer server.Close()

	got, err := getVotingPower(context.Background(), server.URL+"/", ToBytes("abcd"))
	if err != nil {
		t.Fatal(err)
	}
	if got != (votingPower{power: 300, total: 1000, rank: 2, size: 3}) {
		t.Fatalf("got %+v", got)
	}

	got, err = getVotingPower(context.Background(), server.URL, ToBytes("ffff"))
	if err != nil || got.power != 0 || got.rank != 0 || got.size != 3 {
		t.Fatalf("validator outside the set: %+v, %v", got, err)
	}
}
