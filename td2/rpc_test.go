package tenderduty

import (
	"testing"
	"time"
)

func TestNodeLag(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		headHeight int64
		headSeen   time.Time
		nodeHeight int64
		nodeBlock  time.Time
		behind     int64
		lag        time.Duration
		stuck      bool
	}{
		{"stuck", 2000, now, 1000, now.Add(-3 * time.Minute), 1000, 3 * time.Minute, true},
		{"caught up", 2000, now, 2000, now.Add(-3 * time.Minute), 0, 0, false},
		{"chain stalled", 2000, now.Add(-3 * time.Minute), 1000, now.Add(-4 * time.Minute), 1000, 4 * time.Minute, false},
		{"recent block", 2000, now, 1999, now.Add(-time.Minute), 1, time.Minute, false},
		{"zero head", 0, now, 1000, now.Add(-3 * time.Minute), 0, 0, false},
		{"unseen head", 2000, time.Time{}, 1000, now.Add(-3 * time.Minute), 1000, 3 * time.Minute, false},
		{"future block", 2000, now, 1999, now.Add(time.Minute), 1, 0, false},
		{"node ahead", 2000, now, 2001, now.Add(-3 * time.Minute), 0, 0, false},
		{"block at cutoff", 2000, now, 1000, now.Add(-nodeStuckAfter), 1000, nodeStuckAfter, false},
		{"head at cutoff", 2000, now.Add(-nodeStuckAfter), 1000, now.Add(-3 * time.Minute), 1000, 3 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			behind, lag, stuck := nodeLag(tc.headHeight, tc.headSeen, tc.nodeHeight, tc.nodeBlock, now)
			if behind != tc.behind || lag != tc.lag || stuck != tc.stuck {
				t.Fatalf("got (%d, %s, %t), want (%d, %s, %t)", behind, lag, stuck, tc.behind, tc.lag, tc.stuck)
			}
		})
	}
}
