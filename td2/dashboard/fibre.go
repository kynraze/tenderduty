package dash

import (
	"encoding/json"
	"net/http"
)

// FibrePoint a nil field is an interval with no observation.
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

// hide_logs removes failure details; runtime metrics and version stay public.
func (f *FibreStatus) withoutDetails() *FibreStatus {
	if f == nil {
		return nil
	}
	copy := *f
	copy.DownReason, copy.IdentityError = "", ""
	return &copy
}

type FibreNotification struct {
	MessageType string `json:"msgType"`
	Name        string `json:"name"`
	MetricsAt   int64  `json:"metrics_at"`
}

func fibreNotification(status *ChainStatus) []byte {
	if status == nil || status.MsgType != "fibre" || status.Fibre == nil {
		return nil
	}
	body, _ := json.Marshal(FibreNotification{MessageType: "fibre", Name: status.Name, MetricsAt: status.Fibre.MetricsAt})
	return body
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
	return historyHandler(provider)
}

func historyHandler[T any](provider func(string, string) (T, error)) http.HandlerFunc {
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
