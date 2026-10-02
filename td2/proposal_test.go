package tenderduty

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func wsReply(value string) *WsReply {
	reply := &WsReply{}
	reply.Result.Data.Value = json.RawMessage(value)
	return reply
}

func TestMissedProposalIsDetected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocks, rounds := make(chan *WsReply), make(chan *WsReply)
	results := make(chan StatusUpdate, 8)
	go func() { _ = handleBlocks(ctx, blocks, rounds, results, "ABCD") }()

	// our turn in round 0, another validator made the block
	rounds <- wsReply(`{"height":"101","round":0,"step":"RoundStepNewRound","proposer":{"address":"ABCD","index":"3"}}`)
	blocks <- wsReply(`{"block":{"header":{"height":"101","proposer_address":"OTHER"},"last_commit":{"height":"100","signatures":[{"block_id_flag":2,"validator_address":"ABCD"}]}}}`)
	// only round 1 was ours, that is not a missed proposal
	rounds <- wsReply(`{"height":"102","round":"1","proposer":{"address":"ABCD"}}`)
	blocks <- wsReply(`{"block":{"header":{"height":"102","proposer_address":"OTHER"},"last_commit":{"height":"101","signatures":[{"block_id_flag":2,"validator_address":"ABCD"}]}}}`)
	// our turn and our block
	rounds <- wsReply(`{"height":"103","round":"0","proposer":{"address":"ABCD"}}`)
	blocks <- wsReply(`{"block":{"header":{"height":"103","proposer_address":"ABCD"},"last_commit":{"height":"102","signatures":[{"block_id_flag":2,"validator_address":"ABCD"}]}}}`)

	want := []StatusUpdate{
		{Height: 101, Status: StatusMissedProposal},
		{Height: 100, HeadHeight: 101, Status: StatusSigned, Final: true},
		{Height: 101, HeadHeight: 102, Status: StatusSigned, Final: true},
		{Height: 103, Status: StatusProposed},
		{Height: 102, HeadHeight: 103, Status: StatusSigned, Final: true},
	}
	for _, expected := range want {
		select {
		case got := <-results:
			if got != expected {
				t.Fatalf("got %+v, want %+v", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing update %+v", expected)
		}
	}
}

func TestTrackerKeepsMissedProposals(t *testing.T) {
	tracker := signingTracker{}
	tracker.consume(StatusUpdate{Height: 101, Status: StatusMissedProposal})
	if status, missed, ok := tracker.consume(StatusUpdate{Height: 101, Final: true, Status: StatusSigned}); !ok || !missed || status != StatusMissedProposal {
		t.Fatalf("signed block with a missed proposal: status %d, missed %v", status, missed)
	}
	tracker.consume(StatusUpdate{Height: 102, Status: StatusMissedProposal})
	if status, missed, _ := tracker.consume(StatusUpdate{Height: 102, Final: true, Status: Statusmissed}); !missed || status != Statusmissed {
		t.Fatalf("a missed signature should still show as missed: status %d, missed %v", status, missed)
	}
	if _, missed, _ := tracker.consume(StatusUpdate{Height: 103, Final: true, Status: StatusSigned}); missed {
		t.Fatal("missed proposal leaked into the next block")
	}
}

func TestRecentProposalMissesUseTheWindow(t *testing.T) {
	now := time.Now()
	cc := &ChainConfig{proposalMisses: []time.Time{now.Add(-2 * time.Hour), now.Add(-30 * time.Minute), now.Add(-time.Minute)}}
	if misses := cc.recentProposalMisses(time.Hour); misses != 2 {
		t.Fatalf("counted %d misses in the last hour", misses)
	}
	if len(cc.proposalMisses) != 2 {
		t.Fatal("old misses were kept")
	}
}

func TestProposalAlertDefaults(t *testing.T) {
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.yml")
	data := []byte(`
alert_defaults:
  proposal_enabled: yes
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
	if alerts.ProposalMissed != 3 || alerts.ProposalMinutes != 60 || alerts.ProposalPriority != "warning" {
		t.Fatalf("missing proposal defaults: %+v", alerts)
	}
}
