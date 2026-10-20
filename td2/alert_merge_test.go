package tenderduty

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func mergedAlertFixture(t *testing.T, defaults, overrides string) (*Config, *ChainConfig) {
	t.Helper()
	c := auditConfig(t)
	val := &ValInfo{Moniker: "validator", Valcons: "cons", Bonded: true, MaxMissed: 9990}
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", ValAddress: "valoper", PublicFallback: true, valInfo: val, observedBlock: true, blockTime: 6 * time.Second}
	c.Chains[cc.name] = cc
	files := map[string][]byte{cc.name: []byte("alerts:\n" + overrides)}
	if err := applyAlertDefaults(c, []byte("alert_defaults:\n"+defaults), files); err != nil {
		t.Fatal(err)
	}
	if fatal, problems := validateConfig(c); fatal {
		t.Fatalf("invalid config: %v", problems)
	}
	cc.valInfo = val
	return c, cc
}

func TestSigningStagesShareOneIncident(t *testing.T) {
	c, cc := mergedAlertFixture(t, "  consecutive_priority: info\n", "")
	val := cc.valInfo
	stages := []struct {
		consecutive             float64
		missed                  int64
		jailed                  bool
		message, severity, kind string
	}{
		{5, 5, false, "validator is missing blocks on chain-1", "warning", "Missed blocks"},
		{6, 9000, false, "validator is close to jail on chain-1", "critical", "Jail risk"},
		{7, 9990, true, "validator is jailed on chain-1", "critical", "Jailed"},
		{0, 0, false, "", "info", ""},
	}
	for i, stage := range stages {
		val.Missed, val.Jailed = stage.missed, stage.jailed
		cc.statConsecutiveMiss = stage.consecutive
		cc.jailAlarms(val, stage.consecutive, false)
		sent := 0
		for len(c.alertChan) > 0 {
			a := <-c.alertChan
			if a.quiet {
				continue
			}
			sent++
			if a.uniqueId != "consjail" || a.severity != stage.severity {
				t.Fatalf("stage %d: %+v", i, a)
			}
			if stage.message == "" {
				if !a.resolved || a.summary() != "back in the active set" {
					t.Fatalf("recovery %+v", a)
				}
			} else if a.resolved || a.message != stage.message || reportAlarmKind(cc.name, a.message) != stage.kind {
				t.Fatalf("stage %d: %+v", i, a)
			}
			if i == 0 && !strings.Contains(a.summary(), "5 in a row \u00b7 9,985 misses left before jail") {
				t.Fatalf("missing detail: %q", a.summary())
			}
			checkCard(t, stage.message, a.card())
		}
		want := 1
		if stage.message == "" {
			want = 0
		}
		if sent != 1 || alarms.getCount(cc.name) != want {
			t.Fatalf("stage %d: sent %d, active %d", i, sent, alarms.getCount(cc.name))
		}
	}
}

func TestIntermittentMissesOnStrictChain(t *testing.T) {
	c, cc := mergedAlertFixture(t, "", "")
	cc.valInfo.Window = 10000
	cc.valInfo.MaxMissed = maxMissed(10000, sdk.MustNewDecFromStr("0.8"))
	cc.valInfo.Missed = 1800
	cc.jailAlarms(cc.valInfo, 0, false)
	if len(c.alertChan) != 1 {
		t.Fatalf("got %d alerts", len(c.alertChan))
	}
	a := <-c.alertChan
	if a.message != "validator is close to jail on chain-1" || a.severity != "critical" {
		t.Fatalf("intermittent risk: %+v", a)
	}
}

func TestSigningStageSwitches(t *testing.T) {
	for _, tc := range []struct {
		settings               string
		missed, risk, inactive bool
	}{
		{"", true, true, true},
		{"  consecutive_enabled: no\n", false, true, true},
		{"  alert_if_inactive: no\n", true, true, false},
		{"  missed_blocks_enabled: no\n", false, false, false},
		{"  missed_blocks_enabled: no\n  consecutive_enabled: yes\n  alert_if_inactive: yes\n", false, false, false},
		{"  consecutive_enabled: no\n  alert_if_inactive: no\n", false, true, false},
	} {
		t.Run(strings.TrimSpace(tc.settings), func(t *testing.T) {
			_, cc := mergedAlertFixture(t, "", tc.settings)
			a := cc.Alerts
			if a.consecutiveEnabled() != tc.missed || a.jailRiskEnabled() != tc.risk || a.jailInactiveEnabled() != tc.inactive {
				t.Fatalf("switches: %+v", a)
			}
		})
	}
}

func TestSigningRecoveryAndDisabledCleanup(t *testing.T) {
	defer func(d time.Duration) { missedRecoverAfter = d }(missedRecoverAfter)
	missedRecoverAfter = 0
	for _, disable := range []bool{false, true} {
		c, cc := mergedAlertFixture(t, "", "")
		cc.valInfo.Missed, cc.statConsecutiveMiss = 100, 5
		cc.jailAlarms(cc.valInfo, 5, false)
		<-c.alertChan
		cc.statConsecutiveMiss = 0
		if disable {
			cc.Alerts.MissedBlocksAlerts = false
			cc.valInfo.ValidatorStale, cc.valInfo.SigningStale = true, true
		}
		cc.jailAlarms(cc.valInfo, 0, false)
		a := <-c.alertChan
		if !a.resolved || a.quiet != disable || !disable && a.summary() != "signing again \u00b7 9,890 misses left" {
			t.Fatalf("recovery: %+v / %q", a, a.summary())
		}
	}
}

func TestDeprecatedAlertWarningsOnce(t *testing.T) {
	c, cc := mergedAlertFixture(t, "  percentage_enabled: yes\n  percentage_missed: 1\n  consecutive_priority: info\n", "  percentage_missed: 2\n")
	c.Chains["Other"] = &ChainConfig{}
	c.configWarnings = nil
	if err := applyAlertDefaults(c, []byte("alert_defaults:\n  percentage_enabled: yes\n  percentage_priority: info\n  consecutive_priority: info\n"), map[string][]byte{
		"Chain": []byte("alerts:\n  percentage_missed: 2\n"),
		"Other": []byte("alerts:\n  percentage_missed: 3\n"),
	}); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, warning := range c.configWarnings {
		if warning == "warning: percentage_* is replaced by jail_risk_percent, see docs/config.md" {
			count++
		}
	}
	if count != 1 || len(c.configWarnings) != 2 || !strings.Contains(c.configWarnings[1], "consecutive_priority, percentage_priority") || cc.Alerts.PercentageAlerts {
		t.Fatalf("warnings %v, percentage=%v", c.configWarnings, cc.Alerts.PercentageAlerts)
	}
	cc.valInfo.Window, cc.valInfo.Missed = 10000, 1000
	cc.jailAlarms(cc.valInfo, 0, false)
	if len(c.alertChan) != 0 {
		t.Fatal("percentage setting raised an alert")
	}
}

func TestLegacySigningAlarmsClearOnLoad(t *testing.T) {
	auditConfig(t)
	directory := t.TempDir()
	configFile, stateFile := filepath.Join(directory, "config.yml"), filepath.Join(directory, "state.json")
	config := "chains:\n  Chain:\n    chain_id: chain-1\n    valoper_address: valoper\n    public_fallback: yes\n"
	if err := os.WriteFile(configFile, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	old := []string{
		"validator has missed 5 blocks on chain-1",
		"validator has missed > 10% of the slashing window's blocks on chain-1",
		"validator has used 90% of the misses allowed before jailing on chain-1",
		"validator will be jailed in under 60 minutes on chain-1 if it keeps missing blocks",
		"validator can unjail on chain-1",
	}
	cache := newAlarmCache()
	cache.AlarmIDs = make(map[string]string)
	cache.AllAlarms["Chain"] = make(map[string]time.Time)
	for _, message := range old {
		cache.AllAlarms["Chain"][message] = time.Now()
		cache.AlarmIDs["Chain\x00"+message] = "saved-" + message
		cache.Outbox = append(cache.Outbox, queuedNotification{Destination: di, pendingRecovery: pendingRecovery{Chain: "Chain", Message: message, ID: "saved-" + message}})
	}
	data, err := json.Marshal(savedState{Alarms: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	c, err := loadConfig(configFile, stateFile, filepath.Join(directory, "chains.d"), &password)
	if err != nil {
		t.Fatal(err)
	}
	defer c.cancel()
	defer c.deliveryCancel()
	if alarms.getCount("Chain") != 0 || len(alarms.Outbox) != 0 {
		t.Fatalf("old alarms or notifications survived: %+v", alarms)
	}
	for len(c.alertChan) > 0 {
		a := <-c.alertChan
		if !a.resolved || !a.quiet || a.uniqueId != "saved-"+a.message {
			t.Fatalf("migration %+v", a)
		}
	}
}

func TestUpgradeFamilySwitchesAndFixedSeverity(t *testing.T) {
	for _, tc := range []struct {
		settings string
		upgrade  bool
	}{
		{"", true},
		{"  upgrade_enabled: no\n", false},
	} {
		c, cc := mergedAlertFixture(t, "", tc.settings)
		if cc.Alerts.UpgradeAlerts != tc.upgrade {
			t.Fatalf("switches %+v", cc.Alerts)
		}
		cc.signalLoaded, cc.signal = true, signalState{Version: 11, Power: 10, Total: 100, Threshold: 67}
		cc.signalAlarms(cc.valInfo, false)
		want := 0
		if tc.upgrade {
			want = 1
		}
		if len(c.alertChan) != want {
			t.Fatalf("signal alerts %d", len(c.alertChan))
		}
		if tc.upgrade {
			if a := <-c.alertChan; a.severity != "warning" {
				t.Fatalf("signal severity %+v", a)
			}
			cc.upgradeLoaded, cc.upgrade = true, &upgradePlan{Name: "v4.0.0", Height: 1000}
			cc.upgradeAlarms(cc.valInfo, false)
			if a := <-c.alertChan; a.severity != "critical" {
				t.Fatalf("upgrade severity %+v", a)
			}
		}
	}
}

func TestMissedProposalSettings(t *testing.T) {
	for _, tc := range []struct {
		settings       string
		enabled        bool
		count, minutes int
		severity       string
	}{
		{"", false, 3, 60, "warning"},
		{"  missed_proposal_enabled: yes\n  missed_proposal_count: 6\n  missed_proposal_minutes: 45\n  missed_proposal_severity: info\n", true, 6, 45, "info"},
	} {
		_, cc := mergedAlertFixture(t, "", tc.settings)
		a := cc.Alerts
		if a.ProposalAlerts != tc.enabled || a.ProposalMissed != tc.count || a.ProposalMinutes != tc.minutes || a.ProposalSeverity != tc.severity {
			t.Fatalf("settings: %+v", a)
		}
		if !strings.Contains(cc.proposalMessage("validator"), "block proposals") {
			t.Fatal("message does not identify block proposals")
		}
	}
}

func TestAlertFamilyListAndSettingKeys(t *testing.T) {
	_, cc := mergedAlertFixture(t, "", "  missed_proposal_enabled: yes\n  fibre_enabled: yes\n")
	if got := strings.Join(enabledAlertFamilies(cc.Alerts), ","); got != "missed_blocks,stalled,no_servers,gov,upgrade,missed_proposal,fibre" {
		t.Fatal(got)
	}
	for _, key := range []string{
		"missed_blocks_enabled", "missed_proposal_enabled", "missed_proposal_count", "missed_proposal_minutes", "missed_proposal_severity",
		"gov_severity", "fibre_severity", "upgrade_enabled", "consecutive_enabled", "consecutive_priority", "alert_if_inactive", "alert_if_no_servers",
		"percentage_enabled", "percentage_missed", "percentage_priority", "stalled_enabled", "stalled_minutes",
	} {
		value := "1"
		if strings.HasSuffix(key, "_enabled") || strings.HasPrefix(key, "alert_if") {
			value = "yes"
		}
		if strings.HasSuffix(key, "_priority") || strings.HasSuffix(key, "_severity") {
			value = "warning"
		}
		if err := validateChainSettings([]string{"alerts." + key + "=" + value}); err != nil {
			t.Fatalf("key %s: %v", key, err)
		}
	}
	err := validateChainSettings([]string{"alerts.gov_priority=info"})
	if err == nil || !strings.Contains(err.Error(), "did you mean alerts.gov_severity") {
		t.Fatalf("gov_priority: %v", err)
	}
	for _, key := range []string{"signal_enabled", "jail_enabled", "jail_risk_enabled", "proposal_enabled", "upgrade_priority"} {
		if err := validateChainSettings([]string{"alerts." + key + "=yes"}); err == nil {
			t.Fatalf("%s is accepted", key)
		}
	}
	_, cc = mergedAlertFixture(t, "  missed_blocks_enabled: no\n  stalled_enabled: no\n  alert_if_no_servers: no\n  gov_enabled: no\n  upgrade_enabled: no\n  missed_proposal_enabled: no\n  fibre_enabled: no\n", "")
	if got := enabledAlertFamilies(cc.Alerts); len(got) != 0 {
		t.Fatalf("disabled families: %v", got)
	}
}

func TestJailTimeStageBeforeConsecutiveThreshold(t *testing.T) {
	c, cc := mergedAlertFixture(t, "", "")
	cc.valInfo.Missed = cc.valInfo.MaxMissed - 100
	cc.Alerts.JailRiskPercent = 100
	cc.jailAlarms(cc.valInfo, 1, false)
	if len(c.alertChan) != 1 {
		t.Fatalf("got %d alerts", len(c.alertChan))
	}
	if a := <-c.alertChan; a.severity != "critical" || jailStage(a.message) != "risk" {
		t.Fatalf("time to jail: %+v", a)
	}
}

func TestSigningInactiveReportAndRestoredID(t *testing.T) {
	c, cc := mergedAlertFixture(t, "", "")
	message := "validator is missing blocks on chain-1"
	alarms.AllAlarms[cc.name] = map[string]time.Time{message: time.Now()}
	alarms.AlarmIDs = map[string]string{cc.name + "\x00" + message: "saved-id"}
	cc.valInfo.Bonded = false
	cc.jailAlarms(cc.valInfo, 0, false)
	sent := 0
	for len(c.alertChan) > 0 {
		a := <-c.alertChan
		if a.uniqueId != "saved-id" {
			t.Fatalf("stage id changed: %+v", a)
		}
		if a.quiet {
			continue
		}
		sent++
		if a.resolved || a.severity != "critical" || reportAlarmKind(cc.name, a.message) != "Inactive" {
			t.Fatalf("inactive stage: %+v", a)
		}
	}
	if sent != 1 {
		t.Fatalf("sent %d messages", sent)
	}
}
