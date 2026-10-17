package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type BridgeConfig struct {
	URL       string `yaml:"url"`
	TokenFile string `yaml:"token_file"`
	token     string
}

var bridgePruneWindow = time.Hour
var bridgeMetricsWindow = 5 * time.Minute

type bridgeState struct {
	LastPruned, FailedPrunes              int64
	PrunerKnown                           bool
	Archival, ModeKnown                   bool
	PrunedAt, FailingSince                time.Time
	PrunedHead                            int64
	MetricsAt, StartedAt, NoPeersSince    time.Time
	Version                               string
	FullPeers, ArchivalPeers              int64
	PeersKnown                            bool
	totals                                map[string]float64
	resources                             map[string]float64
	series                                map[string]bridgeSeries
	responses                             []bridgeDelta
	Loaded, Up, TokenRejected             bool
	Failures                              int
	Head, Lag, Sampled, Network           int64
	HeadTime                              time.Time
	Running, CatchUpDone                  bool
	Reason                                string
	DownSince, BehindSince, SamplingSince time.Time
}

func validateBridge(chain string, cc *ChainConfig) (bool, []string) {
	var problems []string
	fatal := false
	if cc.Alerts.BridgeAlerts && cc.Bridge.URL == "" {
		return true, []string{"error: bridge_enabled on " + chain + " requires bridge.url"}
	}
	if cc.Bridge.URL == "" {
		return false, nil
	}
	u, err := url.Parse(cc.Bridge.URL)
	if err != nil || !validHTTPURL(cc.Bridge.URL) || u.User != nil {
		return true, []string{"error: invalid bridge.url on " + chain}
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback() && !ip.IsPrivate()) {
		port := u.Port()
		if port == "" {
			port = "80"
		}
		problems = append(problems, fmt.Sprintf("warning: bridge on %s uses plain http; make sure a firewall lets only tenderduty reach port %s", chain, port))
	}
	cc.Bridge.token = ""
	if cc.Bridge.TokenFile != "" {
		data, err := os.ReadFile(cc.Bridge.TokenFile)
		if err != nil {
			fatal = true
			problems = append(problems, "error: cannot read bridge token_file on "+chain)
		} else {
			cc.Bridge.token = strings.TrimSpace(string(data))
			if strings.ContainsAny(cc.Bridge.token, "\r\n") {
				fatal = true
				problems = append(problems, "error: invalid bridge token_file on "+chain)
			}
		}
	}
	if cc.Alerts.BridgeLagBlocks < 0 || cc.Alerts.BridgeLagMinutes < 0 || int64(cc.Alerts.BridgeLagMinutes) > int64((1<<63-1)/time.Minute) {
		fatal = true
		problems = append(problems, "error: invalid bridge lag thresholds on "+chain)
	}
	return fatal, problems
}

// Errors contain no server text or request data: either can echo credentials.
func (cc *ChainConfig) bridgeRPC(ctx context.Context, method string, result interface{}) (bool, string) {
	body, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
	}{"2.0", 1, method})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.Bridge.URL, bytes.NewReader(body))
	if err != nil {
		return false, "invalid request"
	}
	req.Header.Set("Content-Type", "application/json")
	if cc.Bridge.token != "" {
		req.Header.Set("Authorization", "Bearer "+cc.Bridge.token)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		if e, ok := err.(net.Error); ok && e.Timeout() {
			return false, "timed out"
		}
		var op *net.OpError
		if errors.As(err, &op) {
			return false, "connection failed: " + op.Err.Error()
		}
		return false, "connection failed"
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return true, "token rejected"
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	var reply struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&reply) != nil {
		return false, "invalid JSON response"
	}
	if len(reply.Error) > 0 && string(reply.Error) != "null" {
		var rpcError struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(reply.Error, &rpcError) == nil && strings.Contains(rpcError.Message, "missing permission") {
			return true, "token rejected"
		}
		return false, "JSON-RPC error"
	}
	if len(reply.Result) == 0 || string(reply.Result) == "null" || json.Unmarshal(reply.Result, result) != nil {
		return false, "invalid RPC result"
	}
	return false, ""
}

func (cc *ChainConfig) refreshBridge(ctx context.Context, now time.Time) {
	var head struct {
		Header struct {
			Height string    `json:"height"`
			Time   time.Time `json:"time"`
		} `json:"header"`
	}
	var stats struct {
		Sampled     *int64 `json:"head_of_sampled_chain"`
		Network     *int64 `json:"network_head_height"`
		CatchUpDone *bool  `json:"catch_up_done"`
		Running     *bool  `json:"is_running"`
	}
	rejected, reason := cc.bridgeRPC(ctx, "header.LocalHead", &head)
	rejectedStats, reasonStats := cc.bridgeRPC(ctx, "das.SamplingStats", &stats)
	if ctx.Err() != nil {
		return
	}
	rejected = rejected || rejectedStats
	if reason == "" {
		reason = reasonStats
	}
	height, err := strconv.ParseInt(head.Header.Height, 10, 64)
	if reason == "" && (err != nil || height < 0 || head.Header.Time.IsZero() || stats.Sampled == nil || stats.Network == nil || stats.Running == nil || stats.CatchUpDone == nil || *stats.Sampled < 0 || *stats.Network < 0) {
		reason = "invalid RPC result"
	}
	settings := cc.alertSettings()
	cc.stateMux.Lock()
	var history map[string]float64
	defer func() {
		cc.stateMux.Unlock()
		if history != nil {
			td.bridgeData().record(cc.name, now, history, "")
		}
	}()
	s := &cc.bridge
	s.Loaded, s.Up, s.TokenRejected = true, reason == "", rejected
	if reason != "" {
		s.Failures++
		s.Reason = reason
		if s.DownSince.IsZero() {
			s.DownSince = now
		}
		s.BehindSince, s.SamplingSince, s.NoPeersSince = time.Time{}, time.Time{}, time.Time{}
		return
	}
	s.Failures, s.Reason, s.DownSince = 0, "", time.Time{}
	s.Head, s.HeadTime = height, head.Header.Time
	s.Lag = 0
	if cc.lastBlockNum > 0 {
		s.Lag = max(cc.lastBlockNum-height, 0)
	}
	s.Sampled, s.Network, s.Running, s.CatchUpDone = *stats.Sampled, *stats.Network, *stats.Running, *stats.CatchUpDone
	track := func(since *time.Time, bad bool) {
		if !bad {
			*since = time.Time{}
		} else if since.IsZero() {
			*since = now
		}
	}
	chainDown := cc.noNodes || !cc.lastBlockTime.IsZero() && settings.Stalled > 0 && now.Sub(cc.lastBlockTime) >= time.Duration(settings.Stalled)*time.Minute
	track(&s.BehindSince, cc.lastBlockNum > 0 && !chainDown && cc.lastBlockNum-s.Head > settings.BridgeLagBlocks)
	track(&s.SamplingSince, !s.Running || s.Network-s.Sampled > settings.BridgeLagBlocks)
	sampling := float64(max(s.Network-s.Sampled, 0))
	history = map[string]float64{"head_lag": float64(s.Lag), "head_lag_max": float64(s.Lag), "sample_lag": sampling, "sample_lag_max": sampling}
	if hours, ok := cc.bridgeRetainedHours(); ok {
		history["retained_hours"] = hours
	}
}

func (cc *ChainConfig) watchBridge(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		cc.refreshBridge(ctx, time.Now())
		if ctx.Err() != nil {
			return
		}
		if td.settings().EnableDash {
			select {
			case td.updateChan <- cc.dashboardStatus():
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func isBridgeMessage(message string) bool {
	return strings.HasPrefix(message, "Bridge on ") || strings.HasPrefix(message, "Bridge sampling on ")
}

func bridgeMetricKind(kind string) bool {
	return kind == "metrics_stopped" || kind == "no_peers" || kind == "serving" || kind == "prune_stalled" || kind == "prune_failing"
}

func bridgeKind(message string) string {
	switch {
	case strings.HasPrefix(message, "Bridge sampling on "):
		return "sampling"
	case strings.Contains(message, " has too few full peers") || strings.Contains(message, " has no full peers"):
		return "no_peers"
	case strings.Contains(message, " is failing to serve data"):
		return "serving"
	case strings.Contains(message, " stopped sending metrics"):
		return "metrics_stopped"
	case strings.Contains(message, " stopped pruning"):
		return "prune_stalled"
	case strings.Contains(message, " is failing to prune"):
		return "prune_failing"
	case strings.Contains(message, " rejected the token"):
		return "token"
	case strings.Contains(message, " is unreachable:"):
		return "down"
	}
	return "behind"
}

func (cc *ChainConfig) bridgeAlarmID(message string) string {
	return cc.ChainId + ":" + cc.name + ":bridge:" + bridgeKind(message)
}

func bridgeResolution(message, chain string) resolution {
	switch bridgeKind(message) {
	case "metrics_stopped":
		return resolution{"\u2705", "Bridge metrics", "Bridge on " + chain + " sends metrics again"}
	case "no_peers":
		return resolution{"✅", "Bridge peers again", "Bridge on " + chain + " has enough full peers again"}
	case "serving":
		return resolution{"✅", "Bridge serving", "Bridge on " + chain + " serves data again"}
	case "prune_stalled":
		return resolution{"\u2705", "Bridge pruning", "Bridge on " + chain + " is pruning again"}
	case "prune_failing":
		return resolution{"\u2705", "Bridge pruning", "Bridge on " + chain + " prunes again"}
	case "behind":
		return resolution{"✅", "Bridge OK", "Bridge on " + chain + " is back in sync"}
	case "sampling":
		return resolution{"✅", "Bridge OK", "Bridge on " + chain + " sampling resumed"}
	}
	return resolution{"✅", "Bridge OK", "Bridge OK on " + chain}
}

func bridgeLabel(msg *alertMsg) (string, string) {
	if msg.resolved {
		if kind := bridgeKind(msg.message); bridgeMetricKind(kind) {
			r := bridgeResolution(msg.message, "")
			return r.icon, r.label
		}
		return "✅", "Bridge OK"
	}
	return "🌉", map[string]string{"down": "Bridge down", "token": "Bridge token rejected", "behind": "Bridge behind", "sampling": "Bridge sampling stuck", "metrics_stopped": "Bridge metrics stopped", "no_peers": "Bridge too few peers", "serving": "Bridge serving errors", "prune_stalled": "Bridge pruning stalled", "prune_failing": "Bridge failing to prune"}[bridgeKind(msg.message)]
}

func (cc *ChainConfig) bridgeAlarms(reconcile bool) {
	if !cc.alertSettings().BridgeAlerts {
		return
	}
	cc.stateMux.RLock()
	s := cc.bridge
	s.responses = append([]bridgeDelta(nil), s.responses...)
	cc.stateMux.RUnlock()
	if !s.Loaded {
		return
	}
	settings := cc.alertSettings()
	active := activeMessages(cc.name, "Bridge ")
	wanted := make(map[string]string)
	want := func(message string) {
		kind := bridgeKind(message)
		for previous := range active {
			if isBridgeMessage(previous) && bridgeKind(previous) == kind {
				message = previous
				break
			}
		}
		wanted[message] = cc.bridgeAlarmID(message)
	}
	activeKind := func(kind string) bool {
		for message := range active {
			if bridgeKind(message) == kind {
				return true
			}
		}
		return false
	}
	if s.TokenRejected {
		want("Bridge on " + cc.ChainId + " rejected the token, check token_file")
	} else if !s.Up {
		for message := range active {
			if bridgeKind(message) == "down" || bridgeKind(message) == "token" || s.Failures < 3 && !bridgeMetricKind(bridgeKind(message)) {
				wanted[message] = cc.bridgeAlarmID(message)
			}
		}
		if s.Failures >= 3 {
			for message := range wanted {
				if bridgeKind(message) == "token" {
					delete(wanted, message)
				}
			}
			want("Bridge on " + cc.ChainId + " is unreachable: " + s.Reason)
		}
	} else {
		window := time.Duration(settings.BridgeLagMinutes) * time.Minute
		if s.MetricsAt.IsZero() {
			for message := range active {
				if bridgeKind(message) == "metrics_stopped" {
					want(message)
				}
			}
		} else if time.Since(s.MetricsAt) >= bridgeMetricsWindow {
			want("Bridge on " + cc.ChainId + " stopped sending metrics\nlast received " + s.MetricsAt.In(alertLocation()).Format("15:04 MST"))
		}
		if s.metricsFresh(time.Now()) {
			if s.PrunerKnown && s.LastPruned > 0 && !s.PrunedAt.IsZero() && time.Since(s.PrunedAt) >= bridgePruneWindow && s.Head > s.PrunedHead {
				want(fmt.Sprintf("Bridge on %s stopped pruning\nstuck at %s", cc.ChainId, thousands(s.LastPruned)))
			}
			if s.FailedPrunes > 0 && !s.FailingSince.IsZero() && time.Since(s.FailingSince) >= bridgePruneWindow {
				want(fmt.Sprintf("Bridge on %s is failing to prune\n%d blocks failed", cc.ChainId, s.FailedPrunes))
			}
			if s.PeersKnown && s.FullPeers < 2 && !s.NoPeersSince.IsZero() && time.Since(s.NoPeersSince) >= window {
				peers := fmt.Sprintf("%d full peers", s.FullPeers)
				if s.FullPeers == 1 {
					peers = "1 full peer"
				}
				want("Bridge on " + cc.ChainId + " has too few full peers\n" + peers)
			}
			responses, errors := s.servingWindow(time.Now(), window)
			threshold := .5
			if activeKind("serving") {
				threshold = .25
			}
			if responses >= 20 && float64(errors)/float64(responses) >= threshold {
				want(fmt.Sprintf("Bridge on %s is failing to serve data\n%.0f%% of %d requests failed", cc.ChainId, 100*float64(errors)/float64(responses), responses))
			}
		}
		if !s.BehindSince.IsZero() && (activeKind("behind") || time.Since(s.BehindSince) >= window) {
			want(fmt.Sprintf("Bridge on %s is %d blocks behind", cc.ChainId, s.Lag))
		}
		if !s.SamplingSince.IsZero() && (activeKind("sampling") || time.Since(s.SamplingSince) >= window) {
			want(fmt.Sprintf("Bridge sampling on %s is stuck at %d", cc.ChainId, s.Sampled))
		}
	}
	for message := range active {
		if _, ok := wanted[message]; !ok && isBridgeMessage(message) {
			id := cc.bridgeAlarmID(message)
			r := bridgeResolution(message, cc.ChainId)
			if !s.Up {
				r = resolution{"ℹ️", "Bridge check changed", "Bridge on " + cc.ChainId + " check changed"}
			}
			resolve(cc.name, message, r)
			if !s.Up && (bridgeKind(message) == "behind" || bridgeKind(message) == "sampling") || bridgeMetricKind(bridgeKind(message)) && (!s.Up || !s.metricsFresh(time.Now())) {
				td.resolveQuietly(cc.name, message, &id)
				continue
			}
			td.alert(cc.name, message, "info", true, &id)
		}
	}
	for message, id := range wanted {
		if !active[message] || reconcile {
			id := id
			td.alert(cc.name, message, settings.BridgeSeverity, false, &id)
		}
	}
}
