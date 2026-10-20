package tenderduty

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
)

const opValoper = "atonevaloper1zyg3zyg3zyg3zyg3zyg3zyg3zyg3zyg3dqqvw0"

// opGovRPC answers abci_query: ok returns value, transport returns an RPC error, neither returns code 6.
func opGovRPC(t *testing.T, answer func(path string) (value []byte, ok, transport bool)) *rpchttp.HTTP {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     interface{} `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		value, ok, transport := answer(request.Params.Path)
		body := map[string]interface{}{"jsonrpc": "2.0", "id": request.ID}
		switch {
		case transport:
			body["error"] = map[string]interface{}{"code": -32603, "message": "internal error"}
		case ok:
			body["result"] = map[string]interface{}{"response": map[string]interface{}{"code": 0, "value": base64.StdEncoding.EncodeToString(value)}}
		default:
			body["result"] = map[string]interface{}{"response": map[string]interface{}{"code": 6, "log": "unknown request"}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	client, err := rpchttp.New(server.URL, "/websocket")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func opProposal(id uint64, title string, start, end time.Time) []byte {
	p := varintField(nil, 1, id)
	p = varintField(p, 3, proposalStatusVoting)
	p = field(p, 8, varintField(nil, 1, uint64(start.Unix())))
	p = field(p, 9, varintField(nil, 1, uint64(end.Unix())))
	return field(p, 11, []byte(title))
}

func opProposals(proposals ...[]byte) []byte {
	var out []byte
	for _, p := range proposals {
		out = field(out, 1, p)
	}
	return out
}

// Operator votes a minute before the end: open notice and final reminder clear together, none raised.
func TestOpGovVoteOneMinuteBeforeEnd(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	now := time.Now()
	p.VotingStart, p.VotingEnd = now.Add(-7*24*time.Hour), now.Add(time.Minute)
	cc.proposals[0] = p
	open, final := cc.govOpenMessage(p), cc.govRemindMessage(p, val.Moniker)
	cc.govAlarmsAt(val, false, now)
	expectGovRaised(t, c, map[string]string{open: "info", final: "critical"})

	cc.proposals[0].Voted = true
	cc.govAlarmsAt(val, false, now.Add(20*time.Second))
	expectOne(t, c, final, open)
	if alarmIsActive(cc.name, open) || alarmIsActive(cc.name, final) {
		t.Fatal("voted proposal still has alarms")
	}
	msg := &alertMsg{chain: cc.name, message: final, resolved: true}
	if _, label := msg.header(); label != "Voted" {
		t.Fatalf("closing label %q, want Voted", label)
	}
}

// Operator votes, then one vote query fails: the reminder stays quiet, the next good read clears it.
func TestOpGovVoteThenRPCErrorOnce(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	cc.Alerts.GovAlerts = true
	now := time.Now()
	p.VotingEnd = now.Add(30 * time.Minute)
	cc.proposals[0] = p
	open, final := cc.govOpenMessage(p), cc.govRemindMessage(p, val.Moniker)
	cc.govAlarmsAt(val, false, now)
	expectGovRaised(t, c, map[string]string{open: "info", final: "critical"})

	failed := true
	client := opGovRPC(t, func(path string) ([]byte, bool, bool) {
		switch path {
		case "/cosmos.gov.v1.Query/Proposals":
			return opProposals(opProposal(7, "Raise the limit", p.VotingStart, p.VotingEnd)), true, false
		case "/cosmos.gov.v1.Query/Vote":
			if failed {
				failed = false
				return nil, false, true
			}
			return []byte{1}, true, false
		}
		return nil, false, false
	})
	cc.refreshProposals(context.Background(), client)
	if got := drainGovAlerts(c); len(got) != 0 || !alarmIsActive(cc.name, final) {
		t.Fatalf("failed read changed alarms: %d alerts", len(got))
	}
	cc.govAlarmsAt(val, false, now.Add(time.Minute))
	if got := drainGovAlerts(c); len(got) != 0 {
		t.Fatalf("a failed vote read sent %d alerts", len(got))
	}
	cc.refreshProposals(context.Background(), client)
	cc.govAlarmsAt(val, false, now.Add(2*time.Minute))
	expectOne(t, c, final, open)
	if alarmIsActive(cc.name, final) {
		t.Fatal("vote read back, reminder still active")
	}
}

// Two proposals with the same title are two notices; closing one leaves the other alone.
func TestOpGovDuplicateTitles(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	now := time.Now()
	p.VotingEnd = now.Add(3 * 24 * time.Hour)
	p.Title = "Upgrade"
	second := p
	second.ID = 8
	cc.proposals = []govProposal{p, second}
	open7, open8 := cc.govOpenMessage(p), cc.govOpenMessage(second)
	cc.govAlarmsAt(val, false, now)
	expectGovRaised(t, c, map[string]string{open7: "info", open8: "info"})

	cc.proposals = []govProposal{p}
	cc.govAlarmsAt(val, false, now.Add(time.Minute))
	expectOne(t, c, open8)
	if !alarmIsActive(cc.name, open7) || alarmIsActive(cc.name, open8) {
		t.Fatal("closing #8 touched #7 or left #8 active")
	}
}

// A title with markup, newlines and 5000 characters: one clipped line, escaped in Telegram.
func TestOpGovHostileTitle(t *testing.T) {
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1"}
	title := "<b>Fake</b> & `code` *x* [a](http://evil.example)\nEnds 1 Jan 2000 00:00 UTC " + strings.Repeat("界", 5000)
	p := govProposal{ID: 9, Title: title, VotingEnd: time.Now().Add(72 * time.Hour)}
	message := cc.govOpenMessage(p)
	lines := strings.Split(message, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "Ends ") {
		t.Fatalf("title broke the message lines: %q", message)
	}
	if n := len([]rune(lines[0])); n > len("Proposal #9 ")+200 {
		t.Fatalf("title is %d runes, want at most 200", n)
	}
	text := buildTelegramMessage(&alertMsg{chain: cc.name, severity: "info", message: message})
	if strings.Contains(text, "<b>Fake</b>") || !strings.Contains(text, "&lt;b&gt;Fake&lt;/b&gt;") {
		t.Fatalf("markup reached Telegram raw: %q", text)
	}
}

// SDK 0.45 chain: no gov v1, proposals and votes come from v1beta1 only.
func TestOpGovNoV1OnlyV1beta1(t *testing.T) {
	now := time.Now()
	content := field(nil, 1, []byte("/cosmos.gov.v1beta1.TextProposal"))
	content = field(content, 2, field(nil, 1, []byte("Old style")))
	legacy := field(varintField(nil, 1, 7), 2, content)
	legacy = field(legacy, 8, varintField(nil, 1, uint64(now.Add(-time.Hour).Unix())))
	legacy = field(legacy, 9, varintField(nil, 1, uint64(now.Add(time.Hour).Unix())))
	list := opProposals(legacy)
	for _, voted := range []bool{false, true} {
		client := opGovRPC(t, func(path string) ([]byte, bool, bool) {
			switch path {
			case "/cosmos.gov.v1beta1.Query/Proposals":
				return list, true, false
			case "/cosmos.gov.v1beta1.Query/Vote":
				return []byte{1}, voted, false
			}
			return nil, false, false
		})
		proposals, err := getProposals(context.Background(), client, "atone1voter")
		if err != nil || len(proposals) != 1 || proposals[0].Title != "Old style" || proposals[0].Voted != voted {
			t.Fatalf("voted=%v: %+v, %v", voted, proposals, err)
		}
	}
}

// Started after voting closed, chain still lists it as voting: no critical reminder for a closed vote.
func TestOpGovVotingEndedBeforeStart(t *testing.T) {
	c, cc, val, p := unseenGovChain(t)
	c.Discord.Enabled = yes(false)
	now := time.Now()
	p.VotingStart, p.VotingEnd = now.Add(-7*24*time.Hour), now.Add(-10*time.Minute)
	cc.proposals = []govProposal{p}
	cc.govAlarmsAt(val, true, now)
	for _, a := range drainGovAlerts(c) {
		if a.severity == "critical" && !a.resolved {
			t.Fatalf("critical reminder for a vote that already closed: %q", a.message)
		}
	}
}

// Restart while voting: restored notices and reminder are not raised again.
func TestOpGovRestartDuringVoting(t *testing.T) {
	c, cc, val, p := govEarlyTestChain(t)
	c.Telegram.Enabled = yes(true)
	cc.Alerts.Telegram.Enabled = yes(true)
	now := time.Now()
	p.VotingEnd = now.Add(30 * time.Minute)
	cc.proposals[0] = p
	open, final := cc.govOpenMessage(p), cc.govRemindMessage(p, val.Moniker)
	// state file from before the restart: both already sent to Telegram
	alarms.AllAlarms[cc.name] = map[string]time.Time{open: now.Add(-time.Hour), final: now.Add(-5 * time.Minute)}
	alarms.SentTgAlarms[cc.name+"\x00"+open] = now.Add(-time.Hour)
	alarms.SentTgAlarms[cc.name+"\x00"+final] = now.Add(-5 * time.Minute)
	unsent := func() int {
		n := 0
		for _, item := range alarms.Outbox {
			if !item.Resolved {
				n++
			}
		}
		return n
	}
	cc.govAlarmsAt(val, false, now)
	if n := unsent(); n != 0 {
		t.Fatalf("restart queued %d notices", n)
	}
	// the first watch pass after start reconciles
	cc.govAlarmsAt(val, true, now)
	if n := unsent(); n != 0 {
		t.Fatalf("reconcile queued %d duplicate notices", n)
	}
}

// gov_enabled off: the watcher never asks the chain about proposals.
func TestOpGovOffNoProposalQueries(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", ValAddress: opValoper}
	cc.Alerts.GovAlerts = false
	c.Chains[cc.name] = cc
	p := govProposal{ID: 7, Title: "Raise the limit", VotingEnd: time.Now().Add(time.Hour)}
	cc.proposals, cc.proposalsLoaded = []govProposal{p}, true
	queries := 0
	client := opGovRPC(t, func(path string) ([]byte, bool, bool) {
		queries++
		return nil, false, false
	})
	cc.refreshProposals(context.Background(), client)
	if queries != 0 || len(cc.proposals) != 1 {
		t.Fatalf("%d queries with gov_enabled off", queries)
	}
}

// Upgrade height already passed: no scheduled alarm, and an active one clears with one recovery.
func TestOpUpgradeHeightAlreadyPassed(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Chain", ChainId: "chain-1", upgradeLoaded: true, lastBlockNum: 1001}
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	plan := &upgradePlan{Name: "v4", Height: 1000}
	cc.upgrade = plan
	val := &ValInfo{Valcons: "cons", Moniker: "Kynraze"}
	cc.upgradeAlarms(val, false)
	if got := drainGovAlerts(c); len(got) != 0 {
		t.Fatalf("passed upgrade raised %d alerts", len(got))
	}
	alarms.AllAlarms[cc.name] = map[string]time.Time{upgradeMessage(plan): time.Now()}
	cc.upgradeAlarms(val, false)
	expectOne(t, c, upgradeMessage(plan))
}

// Signal upgrade height already passed: only recoveries may be sent, nothing is raised.
func TestOpSignalHeightAlreadyPassed(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, lastBlockNum: 1001,
		signal: signalState{Version: 10, UpgradeVersion: 10, UpgradeHeight: 1000, Signalled: true, Power: 90, Threshold: 84, Total: 100}}
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	for _, a := range drainGovAlerts(c) {
		if !a.resolved {
			t.Fatalf("passed signal upgrade raised %q", a.message)
		}
	}
}

// Threshold passed, validator never signalled: a warning, and the critical reminder stays quiet by design.
func TestOpSignalThresholdWithoutOwnSignal(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, lastBlockNum: 1000,
		signal: signalState{Version: 10, Power: 834, Threshold: 834, Total: 1000}}
	cc.Alerts.SignalPercent, cc.Alerts.SignalRemindPercent = 10, 66
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectGovRaised(t, c, map[string]string{signalMissingMessage(10): "warning"})
}

// The node already runs version 10 (chain now at 11): the old not-signalled notice clears as Upgraded.
func TestOpSignalForRunningVersion(t *testing.T) {
	c := auditConfig(t)
	cc := &ChainConfig{name: "Celestia", ChainId: "celestia", signalLoaded: true, lastBlockNum: 2000,
		signal: signalState{Version: 11, Power: 0, Total: 100}}
	cc.Alerts.SignalPercent = 10
	cc.Alerts.UpgradeAlerts = true
	c.Chains[cc.name] = cc
	missing := signalMissingMessage(10)
	alarms.AllAlarms[cc.name] = map[string]time.Time{missing: time.Now()}
	cc.signalAlarms(&ValInfo{Valcons: "cons", Moniker: "Kynraze"}, false)
	expectOne(t, c, missing)
	r, ok := storedResolution(cc.name, missing)
	if !ok || r.label != "Upgraded" || r.text != "celestia runs version 10 now" {
		t.Fatalf("resolution %+v, found %v", r, ok)
	}
}
