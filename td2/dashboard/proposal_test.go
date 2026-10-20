package dash

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProposalHandler(t *testing.T) {
	handler := ProposalHandler(func(_ context.Context, chain string, id uint64) (ProposalDetail, error) {
		if chain != "Akash" || id != 7 {
			return ProposalDetail{}, errors.New("private detail")
		}
		return ProposalDetail{Chain: chain, ID: id, TallyError: "tally unavailable from this node"}, nil
	})
	for _, tc := range []struct {
		method, query string
		status        int
	}{
		{"GET", "chain=Unknown&id=7", 404}, {"GET", "chain=Akash&id=8", 404},
		{"GET", "chain=Akash&id=bad", 400}, {"GET", "chain=Akash&id=-1", 400},
		{"GET", "chain=Akash&id=0", 400}, {"GET", "chain=Akash&id=18446744073709551616", 400},
		{"GET", "chain=Akash", 400}, {"POST", "chain=Akash&id=7", 405},
		{"GET", "chain=Akash&id=7", 200},
	} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest(tc.method, "/proposal?"+tc.query, nil))
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private detail") {
			t.Fatal("provider details exposed")
		}
		if tc.status == 200 && (!strings.Contains(w.Body.String(), `"tally_error":"tally unavailable from this node"`) || strings.Contains(w.Body.String(), `"tally":`)) {
			t.Fatal(w.Body.String())
		}
	}
}
