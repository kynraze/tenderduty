package tenderduty

import "fmt"

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
	return fmt.Sprintf("head %d (%d behind) · sampled %d", s.Head, s.Lag, s.Sampled), tone
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
