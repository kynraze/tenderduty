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
