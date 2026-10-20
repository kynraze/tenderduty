package tenderduty

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidatorChangeCardLabels(t *testing.T) {
	for _, title := range []string{"Stake changed", "Validator changed"} {
		msg := &alertMsg{transient: true, message: title + "\nStake +71.6% in 1h · 58 → 100 saga\nSelf-bond a → b saga\nCommission x → y\nMin self delegation a → b\nMoniker a → b\nDescription: website\nUnknown change"}
		got := msg.card().Fields
		want := []alertField{
			row("", "Stake", "+71.6% in 1h · 58 → 100 saga"),
			row("", "Self-bond", "a → b saga"),
			row("", "Commission", "x → y"),
			row("", "Min self delegation", "a → b"),
			row("", "Moniker", "a → b"),
			row("", "Description", "website"),
			row("ℹ️", "Note", "Unknown change"),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fields: %#v, want %#v", title, got, want)
		}
	}
	msg := &alertMsg{transient: true, message: "Stake changed\nStake +71.6% in 1h · 58 → 100 saga"}
	if text := strings.Join(msg.card().telegramLines(), "\n"); strings.Contains(text, "Note") || !strings.Contains(text, "Stake") {
		t.Fatalf("stake card: %s", text)
	}
}

func TestReloadFailureCardLabels(t *testing.T) {
	msg := &alertMsg{transient: true, message: "Config reload failed\nerror: invalid config\nwarning: missing endpoint\nwarn: deprecated option\nyaml: line 5: invalid value\n"}
	card := msg.card()
	want := []alertField{
		row("❌", "Error", "invalid config"),
		row("⚠️", "Warning", "missing endpoint"),
		row("⚠️", "Warning", "deprecated option"),
		row("❌", "Error", "yaml: line 5: invalid value"),
	}
	if card.Title != "Previous configuration is still running" || !reflect.DeepEqual(card.Fields, want) {
		t.Fatalf("reload card: %#v", card)
	}
}

func TestAlertDisplayTimezone(t *testing.T) {
	cc := cardChain(t)
	for _, zone := range []string{"Asia/Jakarta", ""} {
		t.Run(zone, func(t *testing.T) {
			td.Reports.Timezone = zone
			at := time.Now().UTC().Truncate(time.Minute).Add(48 * time.Hour)
			stamp := at.Format(govEndFormat)
			want := at.In(alertLocation()).Format("2 Jan 2006 15:04 MST")
			proposal := cc.govOpenMessage(govProposal{ID: 99, VotingEnd: at})
			jail := jailMessage(&ValInfo{Moniker: "Kynraze", Jailed: true, JailedUntil: at}, cc.ChainId)
			upgrade := upgradeMessage(&upgradePlan{Name: "v4", Height: 12_345_678})
			upgradeETAs.Store("Devnet1@12345678", at)
			for _, tc := range []struct {
				message, label string
				resolved       bool
			}{
				{proposal, "Ends", false},
				{jail, "Unjail after", false},
				{upgrade, "Expected", false},
				{proposal, "Voting ends", true},
			} {
				msg := &alertMsg{chain: "Devnet1", message: tc.message, resolved: tc.resolved}
				found := false
				for _, field := range msg.card().Fields {
					if field.Label == tc.label && strings.HasPrefix(field.Value, want) {
						found = true
					}
				}
				if !found || !strings.Contains(msg.summary(), want) {
					t.Fatalf("%s: summary %q, fields %#v", tc.label, msg.summary(), msg.card().Fields)
				}
				if msg.message != tc.message {
					t.Fatal("stored message changed")
				}
			}
			if !strings.Contains(proposal, stamp) || !strings.Contains(jail, stamp) {
				t.Fatal("alarm keys must keep UTC timestamps")
			}
			for _, prefix := range []string{"Voting ended ", "Voted, voting ends "} {
				msg := &alertMsg{chain: "Devnet1", message: proposal, resolved: true}
				r := resolution{"✅", "Voted", "Proposal #99\n" + prefix + stamp}
				resolve(msg.chain, msg.message, r)
				if !strings.Contains(msg.summary(), prefix+want) || !strings.Contains(strings.Join(msg.card().plainLines(), "\n"), want) {
					t.Fatalf("stored resolution display: %q", msg.summary())
				}
				if stored, _ := storedResolution(msg.chain, msg.message); stored.text != r.text {
					t.Fatal("stored resolution changed")
				}
				resolutions.Delete(msg.chain + "\x00" + msg.message)
			}
		})
	}
}
