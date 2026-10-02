package tenderduty

import (
	"strings"
	"testing"
	"time"
)

func TestJailMessages(t *testing.T) {
	until := time.Now().UTC().Truncate(time.Minute).Add(10 * time.Minute)
	jailed := &alertMsg{chain: "AtomOne", severity: "critical", message: jailMessage(&ValInfo{Moniker: "Validator", Jailed: true, JailedUntil: until}, "atomone-1")}
	if icon, label := jailed.header(); icon != "🚨" || label != "Jailed" {
		t.Fatalf("jailed header %s %s", icon, label)
	}
	if got := buildTelegramMessage(jailed); !strings.Contains(got, "Validator is jailed on atomone-1\nCan unjail after ") || !strings.Contains(got, " · <b>9m left</b>") {
		t.Fatalf("jailed message %q", got)
	}
	jailed.resolved = true
	if _, label := jailed.header(); label != "Active again" {
		t.Fatalf("jailed cleared is %q", label)
	}
	tomb := &alertMsg{chain: "AtomOne", severity: "critical", message: jailMessage(&ValInfo{Moniker: "Validator", Jailed: true, Tombstoned: true}, "atomone-1")}
	if icon, label := tomb.header(); icon != "☠️" || label != "Tombstoned" || !strings.Contains(tomb.summary(), "can't be unjailed") {
		t.Fatalf("tombstoned %s %s %q", icon, label, tomb.summary())
	}
	if got := jailMessage(&ValInfo{Moniker: "Validator", JailedUntil: time.Unix(0, 0)}, "atomone-1"); got != "Validator is no longer in the active set on atomone-1" {
		t.Fatalf("inactive is %q", got)
	}
}

// watchAlerts runs a chain's alarm loop and collects what it sends for a while.
func watchAlerts(t *testing.T, c *Config, chain *ChainConfig) map[string]*alertMsg {
	c.Chains["Chain"] = chain
	c.startWorker(chain.watch)
	got := make(map[string]*alertMsg)
	deadline := time.After(2500 * time.Millisecond)
	for {
		select {
		case alert := <-c.alertChan:
			got[alert.message] = alert
		case <-deadline:
			c.cancel()
			c.workers.Wait()
			return got
		}
	}
}

func TestJailedThenCanUnjail(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Jailed: true, Window: 100, JailedUntil: time.Now().Add(-time.Minute).UTC().Truncate(time.Minute)}}
	chain.Alerts.AlertIfInactive = true
	got := watchAlerts(t, c, chain)
	if len(got) != 2 || got["validator can unjail on test-chain"] == nil || got["validator can unjail on test-chain"].severity != "info" {
		t.Fatalf("alerts %v", got)
	}
	for message, alert := range got {
		if strings.HasPrefix(message, "validator is jailed on test-chain\nCan unjail after ") && alert.severity == "critical" {
			return
		}
	}
	t.Fatalf("no jailed alert in %v", got)
}

func TestTombstonedIsItsOwnAlert(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Jailed: true, Tombstoned: true, Window: 100, JailedUntil: time.Now().Add(-time.Minute)}}
	chain.Alerts.AlertIfInactive = true
	got := watchAlerts(t, c, chain)
	if len(got) != 1 || got["validator is tombstoned on test-chain\nPermanent, the validator can't be unjailed"] == nil {
		t.Fatalf("a tombstoned validator sent %v", got)
	}
}

func TestOldInactiveAlarmClearsWhenActive(t *testing.T) {
	c := auditConfig(t)
	old := "validator is no longer active: validator is jailed"
	alarms.AllAlarms["Chain"] = map[string]time.Time{old: time.Now()}
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Valcons: "consensus", Bonded: true, Window: 100}}
	chain.Alerts.AlertIfInactive = true
	got := watchAlerts(t, c, chain)
	if alert := got[old]; alert == nil || !alert.resolved {
		t.Fatalf("the alarm saved by an older version wasn't cleared: %v", got)
	}
}
