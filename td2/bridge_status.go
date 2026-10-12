package tenderduty

import (
	"fmt"
	"time"
)

func (cc *ChainConfig) bridgeStatus() (text, tone string) {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	if cc.Bridge.URL == "" {
		return "", ""
	}
	s := cc.bridge
	switch {
	case !s.Loaded:
		return "not checked yet", "warning"
	case s.TokenRejected:
		return "token rejected", "critical"
	case !s.Up:
		return "unreachable since " + s.DownSince.In(alertLocation()).Format("15:04"), "critical"
	}
	tone = "ok"
	if !s.BehindSince.IsZero() || !s.SamplingSince.IsZero() {
		tone = "warning"
	}
	text = fmt.Sprintf("head %d (%d behind) · sampled %d", s.Head, s.Lag, s.Sampled)
	if s.metricsFresh(time.Now()) {
		if s.Version != "" {
			text += " · " + bridgeVersion(s.Version)
		}
		if s.PeersKnown {
			text += fmt.Sprintf(" · %d full / %d archival peers", s.FullPeers, s.ArchivalPeers)
		}
		if !s.StartedAt.IsZero() {
			text += " · up " + shortDuration(time.Since(s.StartedAt))
		}
	}
	for message := range activeMessages(cc.name, "Bridge on ") {
		if kind := bridgeKind(message); bridgeMetricKind(kind) {
			tone = "warning"
		}
	}
	return text, tone
}

func (cc *ChainConfig) bridgeSummary() string {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	if cc.Bridge.URL == "" {
		return ""
	}
	s := cc.bridge
	switch {
	case !s.Loaded:
		return ""
	case s.TokenRejected:
		return " · bridge token rejected"
	case !s.Up:
		return " · bridge unreachable"
	case !s.BehindSince.IsZero():
		return fmt.Sprintf(" · bridge %d behind", s.Lag)
	case !s.SamplingSince.IsZero():
		return " · bridge sampling stuck"
	}
	return ""
}

type bridgeFact struct{ Icon, Label, Value, Tone string }

func (cc *ChainConfig) bridgeFacts(now time.Time) (title string, facts []bridgeFact) {
	cc.stateMux.RLock()
	s, configured, blockTime := cc.bridge, cc.Bridge.URL != "", cc.blockTime
	cc.stateMux.RUnlock()
	if !configured {
		return "", nil
	}
	fresh := s.metricsFresh(now)
	title = "Bridge"
	if fresh && s.Version != "" {
		title += " " + bridgeVersion(s.Version)
	}
	add := func(icon, label, value, tone string) {
		facts = append(facts, bridgeFact{icon, label, value, tone})
	}
	switch {
	case !s.Loaded:
		add("\u23f3", "Status", "not checked yet", "warning")
		return
	case s.TokenRejected:
		add("\U0001f510", "Token", "rejected, check token_file", "critical")
		return
	case !s.Up:
		down := "no, since " + s.DownSince.In(alertLocation()).Format("15:04")
		if s.Reason != "" {
			down += " (" + s.Reason + ")"
		}
		add("\U0001f4e1", "Reachable", down, "critical")
		return
	}
	warning := func(bad bool) string {
		if bad {
			return "warning"
		}
		return "ok"
	}
	serving, pruning, metricsStopped := false, false, false
	for message := range activeMessages(cc.name, "Bridge on ") {
		switch bridgeKind(message) {
		case "metrics_stopped":
			metricsStopped = true
		case "serving":
			serving = true
		case "prune_stalled", "prune_failing":
			pruning = true
		}
	}
	add("\U0001f4e6", "Head", fmt.Sprintf("%s \u00b7 %d behind", thousands(s.Head), s.Lag), warning(!s.BehindSince.IsZero()))
	sampled := thousands(s.Sampled)
	if !s.Running {
		sampled += " \u00b7 not running"
	}
	add("\U0001f52c", "Sampled", sampled, warning(!s.SamplingSince.IsZero()))
	if fresh {
		add("\U0001f465", "Peers", fmt.Sprintf("%d full / %d archival", s.FullPeers, s.ArchivalPeers), warning(s.FullPeers < 2))
	}
	td.reportsMux.Lock()
	total := reportTotals(td.reportData.Hours[cc.name], now.Add(-24*time.Hour), now)
	td.reportsMux.Unlock()
	if total.BridgeServed > 0 || total.BridgeResponses > 0 || fresh {
		served := humanBytes(int64(total.BridgeServed))
		if total.BridgeResponses > 0 {
			served += fmt.Sprintf(" \u00b7 %.0f%% errors", 100*float64(total.BridgeErrors)/float64(total.BridgeResponses))
		}
		add("\U0001f4e4", "Served (24h)", served, warning(serving))
	}
	if fresh && (s.PrunerKnown || s.ModeKnown) {
		icon, label, value := "\U0001f9f9", "Pruned", "up to "+thousands(s.LastPruned)
		if s.LastPruned == 0 {
			value = "nothing yet, keeps the last 7d 1h"
		}
		if s.Archival {
			icon, label, value = "\U0001f5c4", "Archival", "Q4 trimmed up to "+thousands(s.LastPruned)
			if s.LastPruned == 0 {
				value = "nothing trimmed yet"
			}
		}
		if s.LastPruned > 0 && s.Head > s.LastPruned && blockTime > 0 {
			value += " \u00b7 keeps ~" + reportDuration((time.Duration(s.Head-s.LastPruned) * blockTime).Round(time.Hour))
		}
		if s.FailedPrunes > 0 {
			value += fmt.Sprintf(" \u00b7 %d failed", s.FailedPrunes)
		}
		add(icon, label, value, warning(pruning || s.FailedPrunes > 0))
	}
	if fresh && !s.StartedAt.IsZero() {
		add("\u23f1", "Up", shortDuration(now.Sub(s.StartedAt)), "")
	}
	if s.MetricsAt.IsZero() {
		tone := ""
		if metricsStopped {
			tone = "warning"
		}
		add("\U0001f4c8", "Metrics", "not received, see docs/bridge.md", tone)
	} else if !fresh {
		add("\U0001f4c8", "Metrics", "last "+shortDuration(now.Sub(s.MetricsAt))+" ago", "warning")
	}
	return
}
