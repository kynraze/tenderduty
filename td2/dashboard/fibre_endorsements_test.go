package dash

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHiddenLogsKeepFibreEndorsements(t *testing.T) {
	summary := &FibreStatus{
		DownReason: "private", IdentityError: "private",
		Last24h:      FibreWindow{Endorsed: 43, Missed: 1, Unknown: 2},
		LatestMissed: []FibreMissedBlob{{Height: 123, Commitment: "74656e736c6500", BlobSize: 2048, Time: 100}},
	}
	provider := func(span, chain string) (FibreHistory, error) {
		return FibreHistory{Range: span, Chains: []FibreChainHistory{{Name: "Chain", Summary: summary,
			Series: []FibrePoint{{Time: 100, Metrics: map[string]float64{"endorsements_endorsed": 43, "endorsements_missed": 1}}},
		}}}, nil
	}
	response := httptest.NewRecorder()
	FibreHandler(hideFibreDetails(provider))(response, httptest.NewRequest(http.MethodGet, "/fibre?chain=Chain", nil))
	body := response.Body.String()
	for _, text := range []string{`"endorsed":43`, `"missed":1`, `"unknown":2`, `"latest_missed"`, `"height":123`, `"commitment":"74656e736c6500"`, `"endorsements_missed":1`} {
		if response.Code != http.StatusOK || !strings.Contains(body, text) {
			t.Fatalf("endorsements hidden: %s", body)
		}
	}
	if strings.Contains(body, "private") || summary.DownReason != "private" {
		t.Fatal("details leaked or original summary changed")
	}
}
