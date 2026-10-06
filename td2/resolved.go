package tenderduty

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Recovery text describes the cleared condition; raised messages remain alarm keys.

type resolution struct {
	icon, label, text string
}

// resolutions stores recovery text by chain and message, removed when raised again.
var resolutions sync.Map

func resolve(chain, message string, r resolution) {
	resolutions.Store(chain+"\x00"+message, r)
}

func storedResolution(chain, message string) (resolution, bool) {
	r, ok := resolutions.Load(chain + "\x00" + message)
	if !ok {
		return resolution{}, false
	}
	return r.(resolution), true
}

// withoutSeverity drops the "Severity: x" line node down messages start with.
func withoutSeverity(message string) string {
	if strings.HasPrefix(message, "Severity: ") {
		if _, rest, ok := strings.Cut(message, "\n"); ok {
			return rest
		}
	}
	return message
}

var (
	consecutiveMissed = regexp.MustCompile(`^(.+) has missed \d+ blocks on (\S+)$`)
	percentageMissed  = regexp.MustCompile(`^(.+) has missed > (\d+)% of the slashing window's blocks on (\S+)$`)
	chainStalled      = regexp.MustCompile(`^stalled: have not seen a new block on (\S+) in \d+ minutes$`)
	noRpcNodes        = regexp.MustCompile(`^no RPC endpoints are working for (\S+)$`)
	rpcNodeDown       = regexp.MustCompile(`^RPC node (\S+) has been down for > .+ on (\S+)$`)
	outOfActiveSet    = regexp.MustCompile(`^(.+) is (?:jailed|no longer in the active set) on (\S+)`)
	tombstoned        = regexp.MustCompile(`^(.+) is tombstoned on (\S+)`)
	canUnjail         = regexp.MustCompile(`^(.+) can unjail on (\S+)$`)
	proposalsMissed   = regexp.MustCompile(`^(.+) missed (\d+) block proposals? in (\d+ minutes?) on (\S+)$`)
	jailRisk          = regexp.MustCompile(`^(.+) has used (\d+)% of the misses allowed before jailing on (\S+)$`)
	jailSoon          = regexp.MustCompile(`^(.+) will be jailed in under .+ on (\S+) if it keeps missing blocks$`)
)

// resolvedText handles single-cause recoveries; empty keeps the raised message.
func resolvedText(chain, message string) string {
	if isSignalUpgrade(message) {
		if outcome, ok := upgradeOutcomes.Load(chain + "@" + messageHeight(message)); ok && outcome == "Cancelled" {
			return message + "\nupgrade cancelled or rescheduled"
		}
		return message + "\nupgrade height reached"
	}
	if strings.HasPrefix(message, "Node stopped at the version ") {
		return message + "\nblocks advancing again"
	}
	if strings.HasPrefix(message, "Proposal #") {
		return govResolvedText(chain, message)
	}
	first, _, _ := strings.Cut(withoutSeverity(message), "\n")
	if m := consecutiveMissed.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is signing blocks again on %s", m[1], m[2])
	}
	if m := percentageMissed.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is back under %s%% of the slashing window missed on %s", m[1], m[2], m[3])
	}
	if m := chainStalled.FindStringSubmatch(first); m != nil {
		return m[1] + " is producing blocks again"
	}
	if m := noRpcNodes.FindStringSubmatch(first); m != nil {
		return "RPC endpoints are working again for " + m[1]
	}
	if m := rpcNodeDown.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("RPC node %s is back up on %s", m[1], m[2])
	}
	if m := tombstoned.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is no longer tombstoned on %s", m[1], m[2])
	}
	if m := outOfActiveSet.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is back in the active set on %s", m[1], m[2])
	}
	if m := canUnjail.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s was unjailed on %s", m[1], m[2])
	}
	if m := proposalsMissed.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s missed fewer than %s block proposals in the last %s on %s", m[1], m[2], m[3], m[4])
	}
	if m := jailRisk.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is back under %s%% of the misses allowed before jailing on %s", m[1], m[2], m[3])
	}
	if m := jailSoon.FindStringSubmatch(first); m != nil {
		return fmt.Sprintf("%s is no longer close to being jailed on %s", m[1], m[2])
	}
	return ""
}

// govResolvedText follows govResolvedLabel.
func govResolvedText(chain, message string) string {
	lines := strings.Split(message, "\n")
	ends, moniker := "", ""
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "Ends "):
			ends = strings.TrimPrefix(line, "Ends ")
		case strings.HasSuffix(line, " has not voted yet"):
			moniker = strings.TrimSuffix(line, " has not voted yet")
		case strings.HasSuffix(line, " has not voted"):
			moniker = strings.TrimSuffix(line, " has not voted")
		}
	}
	_, label := govResolvedLabel(chain, message)
	switch {
	case label == "Voted" && moniker != "":
		return lines[0] + "\n" + moniker + " voted"
	case label == "Voted":
		return lines[0] + "\nVoted, voting ends " + ends
	case moniker != "":
		return lines[0] + "\nVoting ended without a vote from " + moniker
	case ends != "":
		if end, err := time.Parse(govEndFormat, ends); err == nil && time.Now().Before(end) {
			return lines[0] + "\nVoting closed"
		}
		return lines[0] + "\nVoting ended " + ends
	}
	return ""
}

// orphanNote is the last line of an alarm cleared because nothing watches it.
const orphanNote = "The alert was turned off or its settings changed"

// orphanResolution is for a restored alarm nothing watches any more.
func orphanResolution(message string) resolution {
	return resolution{"ℹ️", "No longer watched", withoutSeverity(message) + "\n" + orphanNote}
}
