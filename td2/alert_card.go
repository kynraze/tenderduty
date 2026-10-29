package tenderduty

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"
)

// Cards use notification text without changing alarm state.

const (
	dotGreen  = "🟢"
	dotYellow = "🟡"
	dotRed    = "🔴"
	dotWhite  = "⚪"
)

type alertField struct {
	Icon, Label, Value string
	Code               bool // shown as code: hosts, urls and chain ids
}

type alertCard struct {
	Icon, State string
	Title       string // bold line above the fields: proposal or upgrade name
	Fields      []alertField
	Note        string // body of an unknown message, no fields
	Extra       string // the chain's extra_info
}

func (c alertCard) known() bool { return c.Note == "" }

// cardValueLimit caps a field value, e.g. an error from a host.
const cardValueLimit = 300

func row(icon, label, value string) alertField {
	return alertField{Icon: icon, Label: label, Value: value}
}

func codeField(icon, label, value string) alertField {
	return alertField{Icon: icon, Label: label, Value: value, Code: true}
}

func validatorField(moniker string) alertField { return row("👤", "Validator", moniker) }

func chainField(chainId string) alertField { return codeField("⛓", "Chain", chainId) }

func who(moniker, chainId string) []alertField {
	return []alertField{validatorField(moniker), chainField(chainId)}
}

// cardParts separates identity fields from fields that change on recovery.
type cardParts struct {
	title        string
	ident, state []alertField
	used         bool // rule consumed the detail lines
}

// cardRule matches the first line and reads the remaining detail lines.
type cardRule struct {
	re    *regexp.Regexp
	build func(msg *alertMsg, m, rest []string) cardParts
}

func matchRule(msg *alertMsg, rules []cardRule, first string, rest []string) (cardParts, bool) {
	for _, r := range rules {
		m := r.re.FindStringSubmatch(first)
		if m == nil {
			continue
		}
		p := r.build(msg, m, rest)
		if !p.used {
			p.state = append(p.state, noteFields(rest)...)
		}
		return p, true
	}
	return cardParts{}, false
}

var cardOwnNodeBinary = regexp.MustCompile(`^\S+ · (version unknown|celestia-app \S+ · (ready|needs v\d+))$`)

func noteFields(lines []string) []alertField {
	var notes []alertField
	for _, line := range lines {
		if line != "" {
			switch {
			case strings.HasPrefix(line, "Slashed "):
				notes = append(notes, row("🔥", "Slashed", strings.TrimPrefix(line, "Slashed ")))
			case strings.HasPrefix(line, "Double-signed "):
				notes = append(notes, row("✍️", "Double-signed", strings.TrimPrefix(line, "Double-signed ")))
			case strings.HasPrefix(line, "≈ "):
				notes = append(notes, row("🕒", "Expected", strings.TrimPrefix(line, "≈ ")))
			case cardOwnNodeBinary.MatchString(line):
				notes = append(notes, row("🔌", "Node", line))
			case strings.HasPrefix(line, "stopped at the v"), strings.HasPrefix(line, "halted at the "):
				notes = append(notes, row("⬆️", "Upgrade", line))
			case strings.HasPrefix(line, "RPC node ready: "), strings.HasPrefix(line, "RPC node runs "):
				notes = append(notes, row("⬆️", "Binary", line))
			case strings.HasPrefix(line, "jailed since "), strings.HasPrefix(line, "can unjail "):
				for _, part := range strings.Split(line, " · ") {
					if since, ok := strings.CutPrefix(part, "jailed since "); ok {
						notes = append(notes, row("🔒", "Jailed since", since))
					} else if unjail, ok := strings.CutPrefix(part, "can unjail "); ok {
						notes = append(notes, row("🔓", "Can unjail", unjail))
					}
				}
			case strings.HasPrefix(line, "jailed until "):
				notes = append(notes, row("🔒", "Jailed until", strings.TrimPrefix(line, "jailed until ")))
			case strings.Contains(line, " in a row · "):
				notes = append(notes, row("⚠️", "Missed", line))
			case strings.Contains(line, " misses used"):
				notes = append(notes, row("⚠️", "Misses used", line))
			case line == "send the unjail tx", strings.HasPrefix(line, "upgrade celestia-appd to "):
				notes = append(notes, row("⬆️", "To do", line))
			case line == "tombstoned", line == "unbonding", line == "not in the active set", strings.HasPrefix(line, "not in the top "):
				notes = append(notes, row("🚨", "Reason", line))
			case line == "all nodes ready", strings.HasPrefix(line, "upgrade height reached · not upgraded: "), line == "upgrade height reached", line == "upgrade cancelled or rescheduled", line == "blocks advancing again":
				notes = append(notes, row("✅", "Status", line))
			default:
				notes = append(notes, row("ℹ️", "Note", line))
			}
		}
	}
	return notes
}

var (
	cardMissed          = regexp.MustCompile(`^(.+) has missed (\d+) blocks on (\S+)$`)
	cardStalled         = regexp.MustCompile(`^stalled: have not seen a new block on (\S+) in (\d+) minutes$`)
	cardNodeDown        = regexp.MustCompile(`^RPC node (\S+) has been down for > (.+) on (\S+)$`)
	cardTombstoned      = regexp.MustCompile(`^(.+) is tombstoned on (\S+)$`)
	cardJailed          = regexp.MustCompile(`^(.+) is jailed on (\S+)$`)
	cardInactive        = regexp.MustCompile(`^(.+) is no longer in the active set on (\S+)$`)
	cardLegacyInactive  = regexp.MustCompile(`^(.+) is no longer active: validator is (.+)$`)
	cardJailSoon        = regexp.MustCompile(`^(.+) will be jailed in under (.+) on (\S+) if it keeps missing blocks$`)
	cardSignalStart     = regexp.MustCompile(`^Version (\d+) signalling has started$`)
	cardSignalMissing   = regexp.MustCompile(`^Validator has not signalled version (\d+)(?:, upgrade soon)?$`)
	cardSignalDetail    = regexp.MustCompile(`^(?:([\d.]+)% signalled, the upgrade unlocks at ([\d.]+)%: )?upgrade celestia-appd, then send the signal tx$`)
	cardSignalThreshold = regexp.MustCompile(`^Version (\d+) reached the upgrade threshold$`)
	cardNotRegistered   = regexp.MustCompile(`^Validator is not a registered Fibre provider on (\S+)$`)
	cardFibreDown       = regexp.MustCompile(`^Fibre at (\S+) is unreachable on (\S+)$`)
	cardFibreNotReady   = regexp.MustCompile(`^Fibre at (\S+) is not ready on (\S+)$`)
	cardFibreIdentity   = regexp.MustCompile(`^Fibre at (\S+) does not prove it belongs to this validator on (\S+)$`)
	cardSilent          = regexp.MustCompile(`^Fibre has sent no metrics for over (.+) on (\S+)$`)
	cardStorageFull     = regexp.MustCompile(`^Fibre (?:shard budget|storage) is nearly full on (\S+?)(?:,.*)?$`)
	cardSignFailed      = regexp.MustCompile(`^Fibre could not sign payment promises on (\S+)$`)
	cardSigningSlow     = regexp.MustCompile(`^Fibre signing is slower than 10ms on (\S+)$`)
	cardStorageErrors   = regexp.MustCompile(`^Fibre storage operations are failing on (\S+)$`)
	cardObjectErrors    = regexp.MustCompile(`^Fibre storage backend requests are failing on (\S+)$`)
	cardIdle            = regexp.MustCompile(`^Fibre has had no successful upload for over (.+) on (\S+)$`)

	cardUpgrade       = regexp.MustCompile(`^Upgrade (.+) at height ([\d,]+)(?: is close)?$`)
	cardSignalUpgrade = regexp.MustCompile(`^Version (\d+) upgrade at height ([\d,]+)(?: is close)?$`)

	backSigning       = regexp.MustCompile(`^(.+) is signing blocks again on (\S+)$`)
	backWindow        = regexp.MustCompile(`^(.+) is back under (\d+)% of the slashing window missed on (\S+)$`)
	backProducing     = regexp.MustCompile(`^(\S+) is producing blocks again$`)
	backRpc           = regexp.MustCompile(`^RPC endpoints are working again for (\S+)$`)
	backNode          = regexp.MustCompile(`^RPC node (\S+) is back up on (\S+)$`)
	backNotTombstoned = regexp.MustCompile(`^(.+) is no longer tombstoned on (\S+)$`)
	backActive        = regexp.MustCompile(`^(.+) is back in the active set on (\S+)$`)
	backUnjailed      = regexp.MustCompile(`^(.+) was unjailed on (\S+)$`)
	backProposals     = regexp.MustCompile(`^(.+) missed fewer than (\d+) block proposals in the last (\d+ minutes?) on (\S+)$`)
	backJailRisk      = regexp.MustCompile(`^(.+) is back under (\d+)% of the misses allowed before jailing on (\S+)$`)
	backJailSoon      = regexp.MustCompile(`^(.+) is no longer close to being jailed on (\S+)$`)

	backRegistered   = regexp.MustCompile(`^(.+) is a registered Fibre provider on (\S+) at (\S+)$`)
	backUnregistered = regexp.MustCompile(`^Fibre at (\S+) is no longer registered on (\S+)$`)
	backMoved        = regexp.MustCompile(`^Fibre moved from (\S+) to (\S+) on (\S+)$`)
	backFibreReady   = regexp.MustCompile(`^Fibre at (\S+) is ready again on (\S+)$`)
	backFibreUp      = regexp.MustCompile(`^Fibre at (\S+) answers again on (\S+)$`)
	backVerified     = regexp.MustCompile(`^Fibre at (\S+) proves it belongs to (.+) on (\S+)$`)
	backUnchecked    = regexp.MustCompile(`^Fibre at (\S+) is no longer checked, the validator's consensus key isn't known$`)

	backMetrics       = regexp.MustCompile(`^Fibre metrics are arriving again on (\S+)$`)
	backStorage       = regexp.MustCompile(`^Fibre shards are back under (\d+)% of the budget on (\S+), no uploads turned away for (\d+ minutes)$`)
	backSigned        = regexp.MustCompile(`^Fibre is signing payment promises again on (\S+)$`)
	backSigningFast   = regexp.MustCompile(`^Fibre signing is back under 10ms on (\S+)$`)
	backStorageOps    = regexp.MustCompile(`^Fibre storage operations are succeeding again on (\S+)$`)
	backObjectOps     = regexp.MustCompile(`^Fibre storage backend requests are succeeding again on (\S+)$`)
	backUploads       = regexp.MustCompile(`^Fibre stored an upload again on (\S+)$`)
	backUpgraded      = regexp.MustCompile(`^(\S+) runs version (\d+) now$`)
	backSignalled     = regexp.MustCompile(`^(.+) signalled version (\d+) on (\S+)$`)
	backScheduled     = regexp.MustCompile(`^Version (\d+) upgrade is scheduled at height ([\d,]+)$`)
	backFellBack      = regexp.MustCompile(`^Version (\d+) fell back under the upgrade threshold$`)
	backSignalDropped = regexp.MustCompile(`^Version (\d+) signalling dropped below (\d+)%$`)

	cardVoted          = regexp.MustCompile(`^(.+) voted$`)
	cardVotedEnds      = regexp.MustCompile(`^Voted, voting ends (.+)$`)
	cardNoVote         = regexp.MustCompile(`^Voting ended without a vote from (.+)$`)
	cardVotingEnded    = regexp.MustCompile(`^Voting ended (.+)$`)
	cardMetricsLast    = regexp.MustCompile(`^last metrics (.+)$`)
	cardMetricsNone    = regexp.MustCompile(`^no metrics since (.+)$`)
	cardStorageDetail  = regexp.MustCompile(`^(?:(.+?), )?(\d+ uploads?) turned away (in the last 10 minutes)\. (.+)$`)
	cardSignDetail     = regexp.MustCompile(`^(\d+ of \d+ signatures) failed (.+)$`)
	cardSlowDetail     = regexp.MustCompile(`^(\d+ of \d+ signatures) took over 10ms (.+)$`)
	cardStoreDetail    = regexp.MustCompile(`^(put \d+, (?:get \d+, )?prune \d+) failed (in the last .+)$`)
	cardObjectDetail   = regexp.MustCompile(`^([^:]+): (\d+ of \d+ requests failed .+ \(.+\))$`)
	cardLastUploadLine = regexp.MustCompile(`^last successful upload (.+)$`)
)

var raisedRules = []cardRule{
	{regexp.MustCompile(`^Bridge on (\S+) stopped sending metrics$`), func(_ *alertMsg, m, rest []string) cardParts {
		value := "not received"
		if len(rest) > 0 {
			value += " since " + strings.TrimPrefix(rest[0], "last received ")
		}
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Metrics", dotRed+" "+value), row("❓", "Why", "OTel alerts (peers, serving, pruning) are paused")}, used: true}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) stopped pruning$`), func(_ *alertMsg, m, rest []string) cardParts {
		value := "stalled"
		if len(rest) > 0 {
			value = "stalled at " + strings.TrimPrefix(rest[0], "stuck at ")
		}
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Pruning", dotRed+" "+value), row("❓", "Why", "disk keeps growing")}, used: true}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) is failing to prune$`), func(_ *alertMsg, m, rest []string) cardParts {
		value := "blocks failed"
		if len(rest) > 0 {
			value = rest[0]
		}
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Pruning", dotRed+" "+value), row("❓", "Why", "those blocks stay on disk")}, used: true}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) has (too few|no) full peers$`), func(_ *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotRed+" too few full peers")}, used: true}
		if len(rest) > 0 {
			p.state = append(p.state, row("", "Peers", rest[0]))
		}
		return p
	}},
	{regexp.MustCompile(`^Bridge on (\S+) is failing to serve data$`), func(_ *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Serving", dotRed+" most requests failing")}, used: true}
		if len(rest) > 0 {
			p.state = append(p.state, row("", "Requests", rest[0]))
		}
		p.state = append(p.state, row("", "Check", "disk and bridge logs"))
		return p
	}},
	{regexp.MustCompile(`^Bridge on (\S+) is unreachable: (.+)$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotRed+" unreachable"), row("", "Reason", m[2])}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) rejected the token, check token_file$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotRed+" token rejected"), row("", "Check", "token_file")}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) is (\d+) blocks behind$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotYellow+" "+m[2]+" blocks behind")}}
	}},
	{regexp.MustCompile(`^Bridge sampling on (\S+) is stuck at (\d+)$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Sampling", dotYellow+" stuck at "+m[2])}}
	}},
	{regexp.MustCompile(`^Fibre is not endorsing on (\S+)$`), func(_ *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("✍️", "Endorsed", dotRed+" stopped")}, used: true}
		if len(rest) > 0 {
			p.state = append(p.state, row("📊", "Blobs", rest[0]))
		}
		if len(rest) > 1 {
			p.state = append(p.state, row("🔎", "Check", rest[1]))
		}
		return p
	}},
	{cardMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🎯", "Missed", m[2]+" blocks in a row")}}
	}},
	{percentageMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("📉", "Window", "> "+m[2]+"% missed")}}
	}},
	{upgradeStalled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("⬆️", "Upgrade", m[2]+" at height "+m[3]), row("🧱", "Status", m[4])}}
	}},
	{cardStalled, func(_ *alertMsg, m, rest []string) cardParts {
		minutes := m[2] + " minutes"
		if m[2] == "1" {
			minutes = "1 minute"
		}
		p := cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧱", "Blocks", dotRed+" no new block in "+minutes)}}
		if len(rest) > 0 && strings.Contains(rest[0], " is the only source, so either it or the chain stopped") {
			p.state = append(p.state, row("🔌", "Source", rest[0]))
			p.state = append(p.state, noteFields(rest[1:])...)
			p.used = true
		}
		return p
	}},
	{noRpcNodes, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔌", "RPC", dotRed+" no endpoint working")}}
	}},
	{cardNodeDown, func(msg *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🔌", "Node", td.nodeDisplay(msg.chain, m[1])), chainField(m[3])}, state: []alertField{row("📡", "Status", dotRed+" down for > "+m[2])}}
	}},
	{cardTombstoned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2])}
	}},
	{cardJailed, func(msg *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotRed+" jailed")}, used: true}
		for _, line := range rest {
			if after, ok := strings.CutPrefix(line, "Can unjail after "); ok {
				p.state = append(p.state, row("🔓", "Unjail after", after))
			} else {
				p.state = append(p.state, noteFields([]string{line})...)
			}
		}
		return p
	}},
	{cardLegacyInactive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{validatorField(m[1])}, state: []alertField{row("🔒", "Status", dotRed+" "+m[2])}}
	}},
	{cardInactive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotRed+" not in the active set")}}
	}},
	{canUnjail, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔓", "Status", dotYellow+" jail time is over, an unjail transaction is needed")}}
	}},
	{proposalsMissed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[4]), state: []alertField{row("📦", "Proposals", "missed "+m[2]+" in "+m[3])}}
	}},
	{jailRisk, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🚧", "Misses used", m[2]+"% of those allowed before jailing")}}
	}},
	{cardJailSoon, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("⏳", "Jailed in", "under "+m[2]+" if it keeps missing blocks")}}
	}},

	{cardSignalStart, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("📣", "Signalling", dotYellow+" started")}}
	}},
	{cardSignalMissing, func(_ *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🗳", "Your signal", dotRed+" not sent yet")}, used: true}
		for _, line := range rest {
			if detail := cardSignalDetail.FindStringSubmatch(line); detail != nil {
				if detail[1] != "" {
					p.state = append(p.state, row("📣", "Signalled", detail[1]+"% of voting power, the upgrade unlocks at "+detail[2]+"%"))
				}
				p.state = append(p.state, row("⬆️", "To do", "upgrade celestia-appd, then send the signal tx"))
			} else {
				p.state = append(p.state, noteFields([]string{line})...)
			}
		}
		return p
	}},
	{cardSignalThreshold, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🚦", "Threshold", dotYellow+" reached, no upgrade scheduled yet")}}
	}},

	{cardNotRegistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧵", "Registered", dotRed+" no")}}
	}},
	{cardFibreDown, func(msg *alertMsg, m, rest []string) cardParts {
		reach := dotRed + " no"
		if d := fibreDownFor(msg.chain, m[1]); d > 0 {
			reach += ", for " + shortDuration(d)
		}
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: append([]alertField{row("📡", "Reachable", reach)}, whyFields(rest)...), used: true}
	}},
	{cardFibreNotReady, func(_ *alertMsg, m, rest []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])},
			state: append([]alertField{row("🩺", "Ready", dotYellow+" no")}, whyFields(rest)...), used: true}
	}},
	{cardFibreIdentity, func(msg *alertMsg, m, rest []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])},
			state: append([]alertField{row("🔐", "Identity", dotRed+" NOT verified")}, whyFields(rest)...), used: true}
	}},

	{cardSilent, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[2], row("📨", "Metrics", dotRed+" none for over "+m[1]), kindSilent, rest)
	}},
	{cardStorageFull, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🗄", "Shard budget", dotRed+" nearly full"), kindStorage, rest)
	}},
	{cardSignFailed, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("✍️", "Signing", dotRed+" failing"), kindSignFailed, rest)
	}},
	{cardSigningSlow, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🐢", "Signing", dotYellow+" slower than 10 ms"), kindSigning, rest)
	}},
	{cardStorageErrors, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🗄", "Storage", dotRed+" operations failing"), kindStorageErrors, rest)
	}},
	{cardObjectErrors, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[1], row("🪣", "Storage backend", dotRed+" requests failing"), kindObject, rest)
	}},
	{cardIdle, func(msg *alertMsg, m, rest []string) cardParts {
		return metricsParts(m[2], row("💤", "Uploads", dotRed+" none for over "+m[1]), kindIdle, rest)
	}},
}

// whyFields shows the detail line of a down or mismatched Fibre host.
func whyFields(rest []string) []alertField {
	if len(rest) == 0 || rest[0] == "" {
		return nil
	}
	return []alertField{row("❓", "Why", rest[0])}
}

// metricsParts is a Fibre metrics alarm, figures from the detail line.
func metricsParts(chainId string, state alertField, kind string, rest []string) cardParts {
	p := cardParts{ident: []alertField{chainField(chainId)}, state: []alertField{state}, used: true}
	if len(rest) == 0 {
		return p
	}
	line := rest[0]
	var found []alertField
	switch kind {
	case kindSilent:
		if m := cardMetricsLast.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last metrics", m[1])}
		} else if m := cardMetricsNone.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last metrics", "none since "+m[1])}
		}
	case kindStorage:
		if m := cardStorageDetail.FindStringSubmatch(line); m != nil {
			if m[1] != "" {
				found = append(found, row("💾", "Used", strings.TrimSuffix(m[1], " used")))
			}
			found = append(found, row("🚫", "Turned away", m[2]+" "+m[3]), row("ℹ️", "Note", m[4]))
		}
	case kindSignFailed:
		if m := cardSignDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Failed", m[1]+" "+m[2])}
		}
	case kindSigning:
		if m := cardSlowDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Slow", m[1]+" over 10 ms "+m[2])}
		}
	case kindStorageErrors:
		if m := cardStoreDetail.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🎯", "Failed", m[1]+" "+m[2])}
		}
	case kindObject:
		if m := cardObjectDetail.FindStringSubmatch(line); m != nil {
			p.state[0].Label = "Storage backend (" + m[1] + ")"
			found = []alertField{row("🎯", "Failed", m[2])}
		}
		for _, detail := range rest[1:] {
			if m := cardObjectDetail.FindStringSubmatch(detail); m != nil {
				found = append(found, row("🎯", "Failed ("+m[1]+")", m[2]))
			} else {
				found = append(found, row("ℹ️", "Advice", detail))
			}
		}
	case kindIdle:
		if m := cardLastUploadLine.FindStringSubmatch(line); m != nil {
			found = []alertField{row("🕒", "Last upload", m[1])}
		}
	}
	if found == nil {
		found = []alertField{row("📝", "Detail", line)}
	}
	p.state = append(p.state, found...)
	return p
}

// resolvedRules match the texts of resolved.go.
var resolvedRules = []cardRule{
	{regexp.MustCompile(`^Bridge on (\S+) sends metrics again$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Metrics", dotGreen+" received again")}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) (is pruning again|prunes again)$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Pruning", dotGreen+" pruning again")}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) has (enough )?full peers again$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotGreen+" enough full peers again")}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) serves data again$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Serving", dotGreen+" serves data again")}}
	}},
	{regexp.MustCompile(`^Bridge OK on (\S+)$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotGreen+" OK")}}
	}},
	{regexp.MustCompile(`^Bridge on (\S+) (is back in sync|sampling resumed|check changed)$`), func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("\U0001f309", "Bridge", dotGreen+" "+m[2])}}
	}},
	{regexp.MustCompile(`^Fibre endorses again on (\S+)$`), func(_ *alertMsg, m, rest []string) cardParts {
		p := cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("✍️", "Endorsed", dotGreen+" endorsing again")}}
		return p
	}},
	{backSigning, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("✍️", "Signing", dotGreen+" signing blocks again")}}
	}},
	{backWindow, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("📉", "Window", dotGreen+" back under "+m[2]+"% missed")}}
	}},
	{backProducing, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🧱", "Blocks", dotGreen+" producing again")}}
	}},
	{backRpc, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔌", "RPC", dotGreen+" endpoints working again")}}
	}},
	{backNode, func(msg *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🔌", "Node", td.nodeDisplay(msg.chain, m[1])), chainField(m[2])}, state: []alertField{row("📡", "Status", dotGreen+" back up")}}
	}},
	{backNotTombstoned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("☠️", "Status", dotGreen+" no longer tombstoned")}}
	}},
	{backActive, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔒", "Status", dotGreen+" back in the active set")}}
	}},
	{backUnjailed, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("🔓", "Status", dotGreen+" unjailed")}}
	}},
	{backProposals, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[4]), state: []alertField{row("📦", "Proposals", dotGreen+" fewer than "+m[2]+" missed in the last "+m[3])}}
	}},
	{backJailRisk, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🚧", "Misses used", dotGreen+" back under "+m[2]+"%")}}
	}},
	{backJailSoon, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[2]), state: []alertField{row("⏳", "Jailing", dotGreen+" no longer close")}}
	}},

	{backRegistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[3]), validatorField(m[1]), chainField(m[2])}, state: []alertField{row("🧵", "Registered", dotGreen+" yes")}}
	}},
	{backUnregistered, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: []alertField{row("🧵", "Registered", dotWhite+" removed from the registry")}}
	}},
	{backMoved, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[2]), chainField(m[3])}, state: []alertField{codeField("↩️", "Was", m[1])}}
	}},
	{backFibreReady, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: []alertField{row("🩺", "Ready", dotGreen+" yes")}}
	}},
	{backFibreUp, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), chainField(m[2])}, state: []alertField{row("📡", "Reachable", dotGreen+" yes")}}
	}},
	{backVerified, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1]), validatorField(m[2]), chainField(m[3])}, state: []alertField{row("🔐", "Identity", dotGreen+" verified")}}
	}},
	{backUnchecked, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{codeField("🌐", "Host", m[1])},
			state: []alertField{row("🔐", "Identity", dotWhite+" not checked"), row("❓", "Why", "the validator's consensus key isn't known")}}
	}},

	{backMetrics, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("📨", "Metrics", dotGreen+" arriving again")}}
	}},
	{backStorage, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[2])},
			state: []alertField{row("🗄", "Shard budget", dotGreen+" under "+m[1]+"%"), row("🚫", "Turned away", "no uploads for "+m[3])}}
	}},
	{backSigned, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("✍️", "Signing", dotGreen+" signing again")}}
	}},
	{backSigningFast, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🐢", "Signing", dotGreen+" back under 10 ms")}}
	}},
	{backStorageOps, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🗄", "Storage", dotGreen+" operations succeeding")}}
	}},
	{backObjectOps, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🪣", "Storage backend", dotGreen+" requests succeeding")}}
	}},
	{backUploads, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("💤", "Uploads", dotGreen+" arriving again")}}
	}},

	{backUpgraded, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{chainField(m[1])}, state: []alertField{row("🔖", "Version", dotGreen+" version "+m[2]+" is running")}}
	}},
	{backSignalled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: who(m[1], m[3]), state: []alertField{row("🗳", "Signal", dotGreen+" version "+m[2]+" signalled")}}
	}},
	{backScheduled, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1]), row("📏", "Height", m[2])}, state: []alertField{row("⬆️", "Upgrade", dotYellow+" scheduled")}}
	}},
	{backFellBack, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("🚦", "Threshold", dotWhite+" back under")}}
	}},
	{backSignalDropped, func(_ *alertMsg, m, _ []string) cardParts {
		return cardParts{ident: []alertField{row("🔖", "Version", m[1])}, state: []alertField{row("📣", "Signalling", dotWhite+" dropped below "+m[2]+"%")}}
	}},
}

// fibreDownFor is how long the host has been unreachable, 0 if unknown.
func fibreDownFor(chain, host string) time.Duration {
	if td == nil {
		return 0
	}
	td.chainsMux.RLock()
	cc := td.Chains[chain]
	td.chainsMux.RUnlock()
	if cc == nil {
		return 0
	}
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	if cc.fibre.Host != host || cc.fibre.Reachable || cc.fibre.DownSince.IsZero() {
		return 0
	}
	return max(time.Since(cc.fibre.DownSince), 0)
}

func (msg *alertMsg) proposalParts(first string, rest []string, state string) cardParts {
	p := cardParts{title: strings.TrimPrefix(first, "Proposal ")}
	if msg.resolved {
		switch state {
		case "Passed":
			p.state = append(p.state, row("📜", "Result", dotGreen+" passed"))
		case "Rejected", "Failed":
			p.state = append(p.state, row("📜", "Result", dotRed+" "+strings.ToLower(state)))
		}
	}
	for _, line := range rest {
		var m []string
		switch {
		case strings.HasPrefix(line, "Ends "):
			p.ident = append(p.ident, row("🗓", "Ends", strings.TrimPrefix(line, "Ends ")))
		case strings.HasSuffix(line, " has not voted yet"):
			p.state = append(p.state, validatorField(strings.TrimSuffix(line, " has not voted yet")), row("🗳", "Vote", dotRed+" not voted yet"))
		case strings.HasSuffix(line, " has not voted"):
			p.state = append(p.state, validatorField(strings.TrimSuffix(line, " has not voted")), row("🗳", "Vote", dotRed+" not voted yet"))
		case msg.resolved && line == "Voting closed":
			p.state = append(p.state, row("🏁", "Voting", "closed"))
		default:
			if !msg.resolved {
				p.state = append(p.state, noteFields([]string{line})...)
			} else if m = cardVotedEnds.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, row("🗳", "Vote", dotGreen+" voted"), row("🗓", "Voting ends", m[1]))
			} else if m = cardNoVote.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, validatorField(m[1]), row("🗳", "Vote", dotRed+" no vote cast"))
			} else if m = cardVotingEnded.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, row("🏁", "Voting", "ended "+m[1]))
			} else if m = cardVoted.FindStringSubmatch(line); m != nil {
				p.state = append(p.state, validatorField(m[1]), row("🗳", "Vote", dotGreen+" voted"))
			} else {
				p.state = append(p.state, noteFields([]string{line})...)
			}
		}
	}
	p.used = true
	return p
}

// upgradeParts is the card for x/upgrade and x/signal upgrades.
func (msg *alertMsg) upgradeParts(title, height string, rest []string, state string) cardParts {
	p := cardParts{title: title, ident: []alertField{row("📏", "Height", height)}, used: true}
	if msg.resolved {
		status := dotGreen + " upgraded"
		if isSignalUpgrade(msg.message) {
			status = dotGreen + " upgrade height reached"
		}
		if state == "Cancelled" {
			status = dotRed + " cancelled"
		}
		p.state = append(p.state, row("🏁", "Status", status))
	}
	for _, line := range rest {
		if after, ok := strings.CutPrefix(line, "Around "); ok && !msg.resolved {
			p.state = append(p.state, row("🕒", "Expected", after))
		} else {
			p.state = append(p.state, noteFields([]string{line})...)
		}
	}
	return p
}

// parts returns false for a message it doesn't know.
func (msg *alertMsg) parts(first string, rest []string, state string) (cardParts, bool) {
	orphan := msg.resolved && len(rest) > 0 && (rest[len(rest)-1] == orphanNote || rest[len(rest)-1] == removedNote)
	if orphan {
		// orphaned alarm: keep identity fields and show that monitoring stopped
		p, ok := raisedParts(msg, first, rest[:len(rest)-1], state)
		if !ok {
			return cardParts{}, false
		}
		p.state = []alertField{row("👁", "Watching", dotWhite+" stopped, the alert was turned off or its settings changed")}
		if rest[len(rest)-1] == removedNote {
			p.state = []alertField{row("👁", "Watching", dotWhite+" stopped, the chain was removed from the config")}
		}
		return p, true
	}
	if msg.resolved {
		if r, ok := storedResolution(msg.chain, msg.message); ok && (isJailAlarm(msg.message) || isSignalMessage(msg.message)) {
			original, _, _ := strings.Cut(msg.message, "\n")
			p, known := raisedParts(msg, original, nil, state)
			p.state = []alertField{row("✅", "Status", r.text)}
			return p, known
		}
		if !strings.HasPrefix(first, "Proposal #") && !cardUpgrade.MatchString(first) && !cardSignalUpgrade.MatchString(first) {
			if p, ok := matchRule(msg, resolvedRules, first, rest); ok {
				return p, true
			}
			// after a restart, use the raised message when no resolution was saved
			p, ok := raisedParts(msg, first, nil, state)
			p.state = []alertField{row("📌", "Status", dotGreen+" cleared")}
			return p, ok
		}
	}
	return raisedParts(msg, first, rest, state)
}

func raisedParts(msg *alertMsg, first string, rest []string, state string) (cardParts, bool) {
	if cardInactive.MatchString(first) || canUnjail.MatchString(first) {
		p, ok := matchRule(msg, raisedRules, first, nil)
		p.state = append(p.state, noteFields(rest)...)
		p.used = true
		return p, ok
	}
	if strings.Contains(first, " is missing blocks on ") {
		parts := strings.SplitN(first, " is missing blocks on ", 2)
		return cardParts{ident: who(parts[0], parts[1]), state: append([]alertField{row("⚠️", "Status", "missing blocks"), row("⏱", "Recovers", "after "+reportDuration(missedRecoverAfter)+" without another run of missed blocks")}, noteFields(rest)...), used: true}, true
	}
	if strings.Contains(first, " is close to jail on ") {
		parts := strings.SplitN(first, " is close to jail on ", 2)
		return cardParts{ident: who(parts[0], parts[1]), state: append([]alertField{row("🚨", "Status", "close to jail")}, noteFields(rest)...), used: true}, true
	}
	if strings.HasPrefix(first, "Node stopped at the version ") {
		return cardParts{ident: []alertField{row("🔖", "Version", fmt.Sprint(signalMessageVersion(first)))}, state: append([]alertField{row("🚨", "Status", "node stopped at upgrade height")}, noteFields(rest)...), used: true}, true
	}
	if strings.HasPrefix(first, "Proposal #") {
		return msg.proposalParts(first, rest, state), true
	}
	if m := cardUpgrade.FindStringSubmatch(first); m != nil {
		return msg.upgradeParts(m[1], m[2], rest, state), true
	}
	if m := cardSignalUpgrade.FindStringSubmatch(first); m != nil {
		return msg.upgradeParts("Version "+m[1], m[2], rest, state), true
	}
	return matchRule(msg, raisedRules, first, rest)
}

func (msg *alertMsg) card() alertCard {
	var c alertCard
	if msg.group != nil {
		c = msg.group.card(msg)
	} else {
		c = msg.cardFrom(msg.rawSummary())
	}
	if msg.escalated && !msg.resolved {
		c.State = "⏫ Escalated · " + c.State
		if !msg.escalatedAt.IsZero() {
			c.Fields = append(c.Fields, row("⏱", "Unresolved for", fmt.Sprintf("%d min", int(time.Since(msg.escalatedAt).Minutes()))))
		}
	}
	return c
}

func (msg *alertMsg) cardFrom(body string) alertCard {
	icon, state := msg.header()
	c := alertCard{Icon: icon, State: state, Extra: msg.extra}
	if msg.transient && strings.HasPrefix(body, "Config reload failed\n") {
		c.Title = "Previous configuration is still running"
		for _, line := range strings.Split(strings.TrimPrefix(body, "Config reload failed\n"), "\n") {
			if line == "" {
				continue
			}
			if rest, ok := strings.CutPrefix(line, "warning: "); ok {
				c.Fields = append(c.Fields, row("⚠️", "Warning", rest))
			} else if rest, ok := strings.CutPrefix(line, "warn: "); ok {
				c.Fields = append(c.Fields, row("⚠️", "Warning", rest))
			} else {
				c.Fields = append(c.Fields, row("❌", "Error", strings.TrimPrefix(line, "error: ")))
			}
		}
		return c
	}
	if title := validatorChangeTitle(body); msg.transient && title != "" {
		for _, line := range strings.Split(strings.TrimPrefix(body, title+"\n"), "\n") {
			label, value := "", ""
			for _, name := range []string{"Stake", "Self-bond", "Commission", "Min self delegation", "Moniker", "Description"} {
				prefix := name + " "
				if name == "Description" {
					prefix = name + ": "
				}
				if rest, ok := strings.CutPrefix(line, prefix); ok {
					label, value = name, rest
					break
				}
			}
			if label != "" {
				c.Fields = append(c.Fields, row("", label, value))
			} else {
				c.Fields = append(c.Fields, noteFields([]string{line})...)
			}
		}
		return c
	}
	if msg.transient && strings.HasPrefix(body, "Proposals already in voting\n") {
		c.Icon = "🗳"
		for _, line := range strings.Split(strings.TrimPrefix(body, "Proposals already in voting\n"), "\n") {
			if chain, proposal, ok := strings.Cut(line, " #"); ok {
				c.Fields = append(c.Fields, row("", chain, "#"+proposal))
			} else {
				c.Fields = append(c.Fields, noteFields([]string{line})...)
			}
		}
		return c
	}
	lines := strings.Split(body, "\n")
	p, ok := msg.parts(lines[0], lines[1:], state)
	if !ok || len(p.ident)+len(p.state) == 0 && p.title == "" {
		c.Note = body
		return c
	}
	c.Title = p.title
	fields := append(append([]alertField(nil), p.ident...), p.state...)
	orphan := msg.resolved && (strings.HasSuffix(body, "\n"+orphanNote) || strings.HasSuffix(body, "\n"+removedNote))
	if !msg.resolved && state != "Tombstoned" && msg.severity != "" && msg.severity != "info" && state != strings.ToUpper(msg.severity[:1])+msg.severity[1:] {
		fields = append(fields, row("⚠️", "Severity", msg.severity))
	}
	if msg.resolved && !orphan && msg.lasted > 0 && msg.wasAProblem() {
		fields = append(fields, row("⏱", "Lasted", shortDuration(msg.lasted)))
	}
	for _, f := range fields {
		if f.Value = clipRunes(strings.TrimSpace(f.Value), cardValueLimit); f.Value != "" {
			c.Fields = append(c.Fields, f)
		}
	}
	if len(c.Fields) == 0 && c.Title == "" {
		c.Note = body
	}
	return c
}

// wasAProblem excludes notices and permanent tombstones from durations.
func (msg *alertMsg) wasAProblem() bool {
	m := msg.message
	return !strings.HasPrefix(m, "Proposal #") && !strings.HasPrefix(m, "Upgrade ") && !isSignalMessage(m) && !strings.Contains(m, " is tombstoned on ")
}

// clipRunes cuts s to n runes, ending in … when longer.
func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (c alertCard) telegramLines() []string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, "<b>"+html.EscapeString(c.Title)+"</b>")
	}
	for _, f := range c.Fields {
		value := html.EscapeString(f.Value)
		if strings.HasSuffix(c.State, "Tombstoned") && f.Label == "Note" && f.Value == "Permanent, the validator can't be unjailed" {
			lines = append(lines, value)
			continue
		}
		if f.Code {
			value = "<code>" + value + "</code>"
		} else {
			value = timeLeft.ReplaceAllString(value, "· <b>$1</b>")
		}
		lines = append(lines, strings.TrimPrefix(statusField(f.Icon, html.EscapeString(f.Label), value), " "))
	}
	if c.Note != "" {
		lines = append(lines, timeLeft.ReplaceAllString(html.EscapeString(c.Note), "· <b>$1</b>"))
	}
	return lines
}

func (c alertCard) markdownLines() []string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, "**"+discordText(c.Title)+"**")
	}
	for _, f := range c.Fields {
		lines = append(lines, strings.TrimSpace(f.Icon+" **"+f.Label+":**")+" "+discordValue(f))
	}
	if c.Note != "" {
		lines = append(lines, timeLeft.ReplaceAllString(discordText(c.Note), "· **$1**"))
	}
	return lines
}

// plainLines is markdownLines without markup, for gotify.
func (c alertCard) plainLines() []string {
	var lines []string
	if c.Title != "" {
		lines = append(lines, c.Title)
	}
	for _, f := range c.Fields {
		lines = append(lines, strings.TrimSpace(f.Icon+" "+f.Label)+": "+f.Value)
	}
	if c.Note != "" {
		lines = append(lines, c.Note)
	}
	return lines
}

func discordText(s string) string {
	return strings.ReplaceAll(s, "[", `\[`)
}

func discordValue(f alertField) string {
	if f.Code {
		return "`" + strings.ReplaceAll(f.Value, "`", "'") + "`"
	}
	return timeLeft.ReplaceAllString(discordText(f.Value), "· **$1**")
}

// discord's limits for an embed, in characters
const (
	discordContentLimit     = 2000
	discordDescriptionLimit = 4096
	discordFieldsLimit      = 25
	discordNameLimit        = 256
	discordValueLimit       = 1024
	discordFooterLimit      = 2048
	discordEmbedLimit       = 6000
)

// inlineLimit is the longest value, in runes, that sits beside others.
const inlineLimit = 24

type DiscordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type DiscordFooter struct {
	Text string `json:"text"`
}

// discordEmbed clips fields to Discord limits.
func (c alertCard) discordEmbed(color uint) DiscordEmbed {
	description := ""
	if c.Title != "" {
		description = "**" + discordText(strings.ReplaceAll(c.Title, "**", "")) + "**"
	}
	if c.Note != "" {
		if description != "" {
			description += "\n"
		}
		description += timeLeft.ReplaceAllString(discordText(c.Note), "· **$1**")
	}
	embed := DiscordEmbed{Description: clipRunes(description, discordDescriptionLimit), Color: color}
	if c.Extra != "" {
		embed.Footer = &DiscordFooter{Text: clipRunes(c.Extra, discordFooterLimit)}
	}
	total := len([]rune(embed.Description))
	if embed.Footer != nil {
		total += len([]rune(embed.Footer.Text))
	}
	for _, f := range c.Fields {
		if len(embed.Fields) == discordFieldsLimit {
			break
		}
		df := DiscordField{
			Name:   clipRunes(strings.TrimSpace(f.Icon+" "+f.Label), discordNameLimit),
			Value:  clipRunes(discordValue(f), discordValueLimit),
			Inline: len([]rune(f.Value)) < inlineLimit,
		}
		if total += len([]rune(df.Name)) + len([]rune(df.Value)); total > discordEmbedLimit {
			break
		}
		embed.Fields = append(embed.Fields, df)
	}
	return embed
}

// SlackField is a legacy attachment field, short ones sit two to a row.
type SlackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// slackText escapes what slack reads as markup.
func slackText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (c alertCard) slackFields() []SlackField {
	fields := make([]SlackField, 0, len(c.Fields))
	for _, f := range c.Fields {
		value := slackText(f.Value)
		if f.Code {
			value = "`" + strings.ReplaceAll(value, "`", "'") + "`"
		}
		fields = append(fields, SlackField{Title: strings.TrimSpace(f.Icon + " " + f.Label), Value: value, Short: len([]rune(f.Value)) < inlineLimit})
	}
	return fields
}

func slackTitle(c alertCard) string {
	if c.Title == "" {
		return ""
	}
	return "*" + strings.ReplaceAll(slackText(c.Title), "*", "") + "*"
}
