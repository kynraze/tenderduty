package dash

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

type ProposalTally struct {
	Yes     float64  `json:"yes"`
	Abstain float64  `json:"abstain"`
	No      float64  `json:"no"`
	Veto    float64  `json:"veto"`
	Turnout *float64 `json:"turnout,omitempty"`
}

type ProposalDetail struct {
	Chain      string         `json:"chain"`
	ChainID    string         `json:"chain_id"`
	Moniker    string         `json:"moniker"`
	ID         uint64         `json:"id"`
	Title      string         `json:"title"`
	Summary    string         `json:"summary"`
	Start      int64          `json:"start"`
	End        int64          `json:"end"`
	Voted      bool           `json:"voted"`
	Tally      *ProposalTally `json:"tally,omitempty"`
	TallyError string         `json:"tally_error"`
}

func ProposalHandler(provider func(context.Context, string, uint64) (ProposalDetail, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id == 0 {
			http.Error(w, "bad proposal id", http.StatusBadRequest)
			return
		}
		result, err := provider(r.Context(), r.URL.Query().Get("chain"), id)
		if err != nil {
			http.Error(w, "proposal not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
