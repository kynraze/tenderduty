package tenderduty

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestMissesBeforeJail(t *testing.T) {
	// AtomOne signs half the window, the Hub only 5%
	for _, tc := range []struct {
		window    int64
		minSigned string
		want      int64
	}{{10_000, "0.5", 5_000}, {10_000, "0.05", 9_500}, {100, "0.333", 67}} {
		if got := maxMissed(tc.window, sdk.MustNewDecFromStr(tc.minSigned)); got != tc.want {
			t.Fatalf("window %d, min signed %s: got %d, want %d", tc.window, tc.minSigned, got, tc.want)
		}
	}
	info := &ValInfo{MaxMissed: 5_000, Missed: 4_000}
	if left := info.missesBeforeJail(); left != 1_000 {
		t.Fatalf("%d left before jail", left)
	}
	info.Missed = 6_000
	if left := info.missesBeforeJail(); left != 0 {
		t.Fatalf("%d left past the limit", left)
	}
}

func TestRiskAlertDefaults(t *testing.T) {
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.yml")
	data := []byte(`
alert_defaults:
  missed_blocks_enabled: yes
chains:
  Juno:
    chain_id: juno-1
    valoper_address: junovaloper1example
    nodes:
      - url: http://localhost:26657
`)
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(configFile, filepath.Join(directory, "state.json"), filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("valid configuration was rejected: %v", problems)
	}
	alerts := c.Chains["Juno"].Alerts
	if alerts.JailRiskPercent != 90 || alerts.JailRiskMinutes != 60 {
		t.Fatalf("missing risk defaults: %+v", alerts)
	}
}

func TestJailRiskAlertFires(t *testing.T) {
	c := auditConfig(t)
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "consensus", Window: 10_000, Missed: 3_000, MaxMissed: 5_000}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.JailRiskPercent = true, 50
	c.Chains["Chain"] = chain
	c.startWorker(chain.watch)
	want := map[string]bool{"validator is close to jail on test-chain": true}
	for len(want) > 0 {
		select {
		case alert := <-c.alertChan:
			if !want[alert.message] || alert.resolved {
				t.Fatalf("unexpected alert %+v", alert)
			}
			delete(want, alert.message)
		case <-time.After(3 * time.Second):
			t.Fatalf("missing alerts: %v", want)
		}
	}
	c.cancel()
	c.workers.Wait()
}

func TestJailSoonAlertFires(t *testing.T) {
	c := auditConfig(t)
	// 100 misses left at 6s a block is 10 minutes away
	chain := &ChainConfig{name: "Chain", ChainId: "test-chain", statConsecutiveMiss: 5, blockTime: 6 * time.Second,
		valInfo: &ValInfo{Moniker: "validator", Bonded: true, Valcons: "consensus", Window: 10_000, Missed: 4_900, MaxMissed: 5_000}}
	chain.Alerts.MissedBlocksAlerts, chain.Alerts.JailRiskPercent, chain.Alerts.JailRiskMinutes = true, 100, 60
	c.Chains["Chain"] = chain
	c.startWorker(chain.watch)
	select {
	case alert := <-c.alertChan:
		if alert.message != "validator is close to jail on test-chain" || alert.severity != "critical" {
			t.Fatalf("unexpected alert %+v", alert)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no jail soon alert")
	}
	c.cancel()
	c.workers.Wait()
}
