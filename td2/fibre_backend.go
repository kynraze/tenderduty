package tenderduty

import (
	"fmt"
	"strings"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

type fibreBackend struct {
	outcomes     map[string]float64
	bytes        float64
	haveBytes    bool
	inFlight     int64
	haveInFlight bool
	window       []fibreDelta
}

func fibreBackendName(attrs []*commonv1.KeyValue) string {
	name, _ := fibreAttr(attrs, "backend")
	if name == "" {
		return "object"
	}
	return strings.ToLower(name)
}

func (st *fibreStore) backendTotal(chain, name string) *fibreBackend {
	if st.backendTotals == nil {
		st.backendTotals = make(map[string]map[string]*fibreBackend)
	}
	if st.backendTotals[chain] == nil {
		st.backendTotals[chain] = make(map[string]*fibreBackend)
	}
	if st.backendTotals[chain][name] == nil {
		st.backendTotals[chain][name] = &fibreBackend{}
	}
	return st.backendTotals[chain][name]
}

func (b *fibreBackend) append(d fibreDelta, now time.Time) {
	kept := b.window[:0]
	for _, old := range b.window {
		if now.Sub(old.at) < fibreKeep {
			kept = append(kept, old)
		}
	}
	b.window = append(kept, d)
	if len(b.window) > fibreMaxDeltas {
		b.window = compactDeltas(b.window)
	}
}

func fibreBackendAdvice(name string) string {
	if name == "local" {
		return "Check Fibre's data disk: space, health and permissions."
	}
	return "Check the bucket's credentials, rate limits, network, and that it is in the same region as Fibre."
}

func fibreBackendDetail(snap fibreSnapshot) string {
	var lines []string
	for _, name := range snap.BackendNames {
		span := snap.BackendSpans[name]
		d := span.Sum
		failed := d.backendTimeout + d.backendThrottled + d.backendError
		if failed == 0 {
			continue
		}
		var outcomes []string
		for _, item := range []struct {
			name  string
			count int64
		}{{"timeout", d.backendTimeout}, {"throttled", d.backendThrottled}, {"error", d.backendError}} {
			if item.count > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%s %d", item.name, item.count))
			}
		}
		lines = append(lines, fmt.Sprintf("%s: %d of %d requests failed %s (%s)", name, failed, d.backendTotal, span.text(), strings.Join(outcomes, ", ")), fibreBackendAdvice(name))
	}
	return strings.Join(lines, "\n")
}
