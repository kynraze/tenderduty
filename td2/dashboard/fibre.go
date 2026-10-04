package dash

import (
	"encoding/json"
	"net/http"
)

// A nil field is an interval with no observation.
type FibrePoint struct {
	Time      int64              `json:"time"`
	Metrics   map[string]float64 `json:"metrics"`
	Occupancy *int64             `json:"occupancy"`
	Budget    *int64             `json:"budget"`
	Reachable *float64           `json:"reachable"`
}

type FibreChainHistory struct {
	Name    string       `json:"name"`
	Summary *FibreStatus `json:"summary"`
	Series  []FibrePoint `json:"series"`
	// p50 and p95 of the whole range in ms, keyed like upload_p95_ms
	Latency map[string]float64 `json:"latency,omitempty"`
}

type FibreHistory struct {
	Range  string              `json:"range"`
	Chains []FibreChainHistory `json:"chains"`
}

// withoutDetails is what hide_logs leaves of a status: the host is on chain, the rest comes from the node or Fibre itself.
func (f *FibreStatus) withoutDetails() *FibreStatus {
	if f == nil {
		return nil
	}
	copy := *f
	copy.DownReason, copy.IdentityError, copy.Version, copy.Resources = "", "", "", nil
	return &copy
}

// hideFibreDetails wraps the history provider for hide_logs.
func hideFibreDetails(provider func(string, string) (FibreHistory, error)) func(string, string) (FibreHistory, error) {
	return func(span, chain string) (FibreHistory, error) {
		history, err := provider(span, chain)
		history.Chains = append([]FibreChainHistory(nil), history.Chains...)
		for i := range history.Chains {
			history.Chains[i].Summary = history.Chains[i].Summary.withoutDetails()
		}
		return history, err
	}
}

func FibreHandler(provider func(string, string) (FibreHistory, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		span := r.URL.Query().Get("range")
		if span == "" {
			span = "24h"
		}
		switch span {
		case "1h", "24h", "7d", "30d":
		default:
			http.Error(w, "unknown range", http.StatusBadRequest)
			return
		}
		data, err := provider(span, r.URL.Query().Get("chain"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		body, err := json.Marshal(data)
		if err != nil {
			http.Error(w, "could not encode history", http.StatusInternalServerError)
			return
		}
		if len(body) > 4<<20 {
			http.Error(w, "select one chain", http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}
