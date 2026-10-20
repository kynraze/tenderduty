package tenderduty

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

type bridgeBucket struct {
	Time    int64              `json:"time"`
	Values  map[string]float64 `json:"values,omitempty"`
	Version string             `json:"version,omitempty"`
}

// All stored fields and their merge rules.
var bridgeFields = map[string]string{
	"head_lag": "last", "head_lag_max": "max", "sample_lag": "last", "sample_lag_max": "max",
	"full_peers": "last", "archival_peers": "last", "full_peers_min": "min",
	"served_bytes": "sum", "responses_success": "sum", "responses_not_found": "sum", "responses_internal_err": "sum",
	"responses_rate_limited": "sum", "responses_peer_err": "sum", "responses_other": "sum",
	"serve_seconds": "sum", "serve_requests": "sum", "put_seconds": "sum", "puts": "sum", "put_failures": "sum",
	"da_lag_seconds": "sum", "da_blocks": "sum", "reclaimed_bytes": "sum", "retained_hours": "last", "failed_prunes": "last",
	"memory_bytes": "last", "memory_max_bytes": "max", "goroutines": "last", "goroutines_max": "max", "restarts": "sum",
}
var bridgeRanges = []string{"1h", "24h", "7d", "30d"}

type bridgeHistoryStore struct {
	mux   sync.Mutex
	tiers map[string]map[string]map[int64]bridgeBucket
	now   func() time.Time
}

func (c *Config) bridgeData() *bridgeHistoryStore {
	c.bridgeOnce.Do(func() {
		c.bridgeStore = &bridgeHistoryStore{tiers: make(map[string]map[string]map[int64]bridgeBucket), now: time.Now}
	})
	return c.bridgeStore
}

func trimBridge(buckets map[int64]bridgeBucket, span string, now time.Time) {
	duration, step := fibreRange(span)
	end := now.UTC().Truncate(step).Unix()
	start := end - int64((duration-step)/time.Second)
	for at := range buckets {
		if at < start || at > end {
			delete(buckets, at)
		}
	}
}

func (st *bridgeHistoryStore) record(chain string, now time.Time, values map[string]float64, version string) {
	st.mux.Lock()
	defer st.mux.Unlock()
	if st.tiers[chain] == nil {
		st.tiers[chain] = make(map[string]map[int64]bridgeBucket)
	}
	for _, span := range bridgeRanges {
		_, step := fibreRange(span)
		buckets := st.tiers[chain][span]
		if buckets == nil {
			buckets = make(map[int64]bridgeBucket)
			st.tiers[chain][span] = buckets
		}
		trimBridge(buckets, span, now)
		at := now.UTC().Truncate(step).Unix()
		b := buckets[at]
		b.Time = at
		if b.Values == nil {
			b.Values = make(map[string]float64)
		}
		for key, v := range values {
			rule, ok := bridgeFields[key]
			if !ok || !validBridgeValue(v) {
				continue
			}
			old, seen := b.Values[key]
			switch rule {
			case "sum":
				v += old
			case "max":
				if seen {
					v = math.Max(v, old)
				}
			case "min":
				if seen {
					v = math.Min(v, old)
				}
			}
			if validBridgeValue(v) {
				b.Values[key] = v
			}
		}
		if version != "" {
			b.Version = clipBytes(version, 64)
		}
		buckets[at] = b
	}
}

func validBridgeValue(v float64) bool { return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

func (st *bridgeHistoryStore) series(chain, span string) []dash.BridgePoint {
	st.mux.Lock()
	defer st.mux.Unlock()
	duration, step := fibreRange(span)
	end := st.now().UTC().Truncate(step)
	buckets := st.tiers[chain][span]
	trimBridge(buckets, span, end)
	points := make([]dash.BridgePoint, 0, int(duration/step))
	for at := end.Add(-duration + step); !at.After(end); at = at.Add(step) {
		p := dash.BridgePoint{Time: at.Unix()}
		if b, ok := buckets[p.Time]; ok {
			p.Version = b.Version
			if len(b.Values) > 0 {
				p.Metrics = make(map[string]float64, len(b.Values)+3)
				for k, v := range b.Values {
					p.Metrics[k] = v
				}
			}
			for _, pair := range [][3]string{{"serve_ms", "serve_seconds", "serve_requests"}, {"put_ms", "put_seconds", "puts"}, {"da_lag_ms", "da_lag_seconds", "da_blocks"}} {
				if n := p.Metrics[pair[2]]; n > 0 {
					average := p.Metrics[pair[1]] / n * 1000
					if validBridgeValue(average) {
						p.Metrics[pair[0]] = average
					}
				}
			}
		}
		points = append(points, p)
	}
	return points
}

func (c *Config) bridgeHistory(span, chain string) (dash.BridgeHistory, error) {
	result := dash.BridgeHistory{Range: span, Chains: make([]dash.BridgeChainHistory, 0)}
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	names := make([]string, 0, len(c.Chains))
	for name := range c.Chains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cc := c.Chains[name]
		if cc != nil && cc.Bridge.URL != "" && (chain == "" || chain == name) {
			result.Chains = append(result.Chains, dash.BridgeChainHistory{Name: name, Series: c.bridgeData().series(cc.name, span)})
		}
	}
	if chain != "" && len(result.Chains) == 0 {
		return result, fmt.Errorf("unknown or unwatched chain")
	}
	return result, nil
}

func (c *Config) savedBridgeHistory() json.RawMessage {
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	st := c.bridgeData()
	st.mux.Lock()
	defer st.mux.Unlock()
	saved := make(map[string]map[string]map[int64]bridgeBucket)
	for name, tiers := range st.tiers {
		if cc := c.Chains[name]; cc != nil && cc.Bridge.URL != "" {
			for span, buckets := range tiers {
				trimBridge(buckets, span, st.now())
			}
			saved[name] = tiers
		}
	}
	data, _ := json.Marshal(saved)
	return data
}

func (c *Config) restoreBridgeHistory(data json.RawMessage) {
	var saved map[string]map[string]map[int64]json.RawMessage
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	st := c.bridgeData()
	now := st.now()
	c.chainsMux.RLock()
	defer c.chainsMux.RUnlock()
	st.mux.Lock()
	defer st.mux.Unlock()
	for name, tiers := range saved {
		if len(name) > 256 {
			continue
		}
		cc := c.Chains[name]
		if cc == nil || cc.Bridge.URL == "" {
			continue
		}
		for span, buckets := range tiers {
			known := false
			for _, r := range bridgeRanges {
				if r == span {
					known = true
				}
			}
			if !known {
				continue
			}
			duration, step := fibreRange(span)
			end := now.UTC().Truncate(step).Unix()
			start := end - int64((duration-step)/time.Second)
			for key, raw := range buckets {
				var b bridgeBucket
				if json.Unmarshal(raw, &b) != nil || b.Time != key || b.Time%int64(step/time.Second) != 0 || b.Time < start || b.Time > end || len(b.Version) > 64 {
					continue
				}
				valid := true
				for k, v := range b.Values {
					if _, ok := bridgeFields[k]; !ok || !validBridgeValue(v) {
						valid = false
						break
					}
				}
				if !valid {
					continue
				}
				if st.tiers[name] == nil {
					st.tiers[name] = make(map[string]map[int64]bridgeBucket)
				}
				if st.tiers[name][span] == nil {
					st.tiers[name][span] = make(map[int64]bridgeBucket)
				}
				st.tiers[name][span][key] = b
			}
		}
	}
}

func (cc *ChainConfig) bridgeRetainedHours() (float64, bool) {
	s := &cc.bridge
	if !s.PrunerKnown || s.LastPruned <= 0 || s.Head <= s.LastPruned || cc.blockTime <= 0 {
		return 0, false
	}
	hours := float64(s.Head-s.LastPruned) * cc.blockTime.Hours()
	return hours, validBridgeValue(hours)
}
