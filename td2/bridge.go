package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
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

type bridgeState struct {
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
	defer cc.stateMux.Unlock()
	s := &cc.bridge
	s.Loaded, s.Up, s.TokenRejected = true, reason == "", rejected
	if reason != "" {
		s.Failures++
		s.Reason = reason
		if s.DownSince.IsZero() {
			s.DownSince = now
		}
		s.BehindSince, s.SamplingSince = time.Time{}, time.Time{}
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

func bridgeKind(message string) string {
	switch {
	case strings.HasPrefix(message, "Bridge sampling on "):
		return "sampling"
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
	case "behind":
		return resolution{"✅", "Bridge OK", "Bridge on " + chain + " is back in sync"}
	case "sampling":
		return resolution{"✅", "Bridge OK", "Bridge on " + chain + " sampling resumed"}
	}
	return resolution{"✅", "Bridge OK", "Bridge OK on " + chain}
}

func bridgeLabel(msg *alertMsg) (string, string) {
	if msg.resolved {
		return "✅", "Bridge OK"
	}
	return "🌉", map[string]string{"down": "Bridge down", "token": "Bridge token rejected", "behind": "Bridge behind", "sampling": "Bridge sampling stuck"}[bridgeKind(msg.message)]
}

func (cc *ChainConfig) bridgeAlarms(reconcile bool) {
	cc.stateMux.RLock()
	s := cc.bridge
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
			if bridgeKind(message) == "down" || bridgeKind(message) == "token" || s.Failures < 3 {
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
			if !s.Up && (bridgeKind(message) == "behind" || bridgeKind(message) == "sampling") {
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
