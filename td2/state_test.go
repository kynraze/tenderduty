package tenderduty

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateFileCanBeReplacedWithoutLosingData(t *testing.T) {
	oldConfig, oldAlarms := td, alarms
	t.Cleanup(func() { td, alarms = oldConfig, oldAlarms })
	td = &Config{Chains: map[string]*ChainConfig{
		"Osmosis": {blocksResults: []int{3, 0}, Nodes: []*NodeConfig{{Url: "http://localhost:26657", down: true, downSince: time.Now()}}},
	}}
	alarms = newAlarmCache()
	alarms.PendingRecoveries["Osmosis\x00missed blocks"] = &pendingRecovery{Chain: "Osmosis", Message: "missed blocks", ID: "validatorconsecutive"}
	stateFile := filepath.Join(t.TempDir(), "state.json")
	for i := 0; i < 2; i++ {
		if err := saveState(stateFile); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Blocks["Osmosis"]) != 2 || len(saved.NodesDown["Osmosis"]) != 1 || len(saved.Alarms.PendingRecoveries) != 1 {
		t.Fatalf("saved state is incomplete: %+v", saved)
	}
}

func TestJailedSinceSurvivesStateRestore(t *testing.T) {
	c := auditConfig(t)
	since := time.Date(2026, 10, 5, 19, 0, 0, 0, time.UTC)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", jailedSince: since}
	c.Chains[cc.name] = cc
	c.Chains["Healthy"] = &ChainConfig{name: "Healthy"}
	directory := t.TempDir()
	stateFile := filepath.Join(directory, "state.json")
	if err := saveState(stateFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.JailedSince) != 1 || !saved.JailedSince["Chain"].Equal(since) {
		t.Fatalf("jail times not saved: %+v", saved.JailedSince)
	}
	configFile := filepath.Join(directory, "config.yml")
	config := "chains:\n  Chain:\n    chain_id: chain-1\n    valoper_address: valoper\n"
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	restored, err := loadConfig(configFile, stateFile, filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.cancel()
	defer restored.deliveryCancel()
	if got := restored.Chains["Chain"].jailSince(); !got.Equal(since) {
		t.Fatalf("restored jail time %v, want %v", got, since)
	}
	next := &ChainConfig{ChainId: cc.ChainId, ValAddress: cc.ValAddress}
	carryChainHistory(cc, next)
	if !next.jailSince().Equal(since) {
		t.Fatal("reload lost jail time")
	}
}
