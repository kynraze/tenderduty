package dash

import "net/http"

type BridgePoint struct {
	Time    int64              `json:"time"`
	Metrics map[string]float64 `json:"metrics"`
	Version string             `json:"version,omitempty"`
}

type BridgeChainHistory struct {
	Name   string        `json:"name"`
	Series []BridgePoint `json:"series"`
}

type BridgeHistory struct {
	Range  string               `json:"range"`
	Chains []BridgeChainHistory `json:"chains"`
}

func BridgeHandler(provider func(string, string) (BridgeHistory, error)) http.HandlerFunc {
	return historyHandler(provider)
}
