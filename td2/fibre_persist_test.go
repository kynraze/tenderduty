package tenderduty

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFibreShortStateRoundTripAndBound(t *testing.T) {
	r := newFibreRig(t)
	end := r.clock.now
	sender := &fibreSender{chain: "Chain", lastReceived: end}
	r.st.senders["sender"] = sender
	for i := 1500; i >= 0; i-- {
		at := end.Add(-time.Duration(i) * time.Minute)
		sender.window = append(sender.window, fibreDelta{
			at: at, uploadOK: 2, known: 1, occupancy: int64(i), budget: 5000,
			haveOccupancy: true, haveBudget: true,
			extra: &fibreExtra{
				Histograms: map[string]fibreHistogram{"upload": {Bounds: []float64{.01, .1}, Buckets: []uint64{1, 1, 0}, Count: 2, Sum: .05}},
				Gauges:     map[string]float64{"go_memory_used_bytes": float64(1000 + i), "go_goroutines": 5},
				Counters:   map[string]float64{"go_memory_allocated_bytes": 10},
			},
		})
		r.st.reach["Chain"] = append(r.st.reach["Chain"], fibreReach{at: at, reachable: i%2 == 0})
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved savedState
	if err := json.Unmarshal(body, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.FibreMinutes)+len(saved.FibreTenmin) > 256<<10 {
		t.Fatalf("short history too large: %d bytes", len(saved.FibreMinutes)+len(saved.FibreTenmin))
	}
	base := saved
	base.FibreMinutes, base.FibreTenmin = nil, nil
	baseBody, err := json.Marshal(base)
	if err != nil || len(body)-len(baseBody) > 256<<10 {
		t.Fatalf("state growth exceeded bound: %d, %v", len(body)-len(baseBody), err)
	}
	fresh := newFibreStore()
	fresh.now = r.clock.Now
	fresh.restoreShort(saved.FibreMinutes, "1h")
	fresh.restoreShort(saved.FibreTenmin, "24h")
	for span, count := range map[string]int{"1h": 60, "24h": 144} {
		if len(fresh.short[span]["Chain"]) != count {
			t.Fatalf("%s has %d buckets", span, len(fresh.short[span]["Chain"]))
		}
		before, beforeLatency := r.st.seriesAndLatency("Chain", span)
		after, afterLatency := fresh.seriesAndLatency("Chain", span)
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeLatency, afterLatency) {
			t.Fatalf("%s changed after restore", span)
		}
		if string(fresh.savedShort(span)) != string(r.st.savedShort(span)) {
			t.Fatalf("%s changed after a second save", span)
		}
	}
	if len(fresh.senders) != 0 || len(fresh.totals) != 0 || len(fresh.histograms) != 0 {
		t.Fatal("restored cumulative baselines or process counters")
	}
	r.advance(20 * time.Minute)
	fresh.senders["live"] = &fibreSender{chain: "Chain", lastReceived: r.clock.now,
		window: []fibreDelta{{at: r.clock.now, uploadOK: 7, known: 1}}}
	for _, span := range []string{"1h", "24h"} {
		before := r.st.series("Chain", span)
		after := fresh.series("Chain", span)
		for i, point := range before {
			if point.Time <= end.Unix() && !reflect.DeepEqual(point, after[i]) {
				t.Fatalf("%s lost pre-restart point %d", span, i)
			}
			if point.Time > end.Unix() && point.Time < r.clock.now.Truncate(time.Minute).Unix() && after[i].Metrics != nil {
				t.Fatalf("%s filled downtime at %d", span, point.Time)
			}
		}
		if after[len(after)-1].Metrics["uploads_ok"] != 7 {
			t.Fatalf("%s lost live data", span)
		}
	}
	r.advance(25 * time.Hour)
	for _, span := range []string{"1h", "24h"} {
		if fresh.savedShort(span) != nil {
			t.Fatalf("%s retained expired buckets", span)
		}
	}
}

func TestFibreShortRestoreValidation(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, span := range []string{"1h", "24h"} {
		duration, step := fibreRange(span)
		at := now.Truncate(step).Unix()
		for _, value := range []string{
			fmt.Sprintf(`{"time":%d,"counts":{"uploads_ok":-1}}`, at),
			fmt.Sprintf(`{"time":%d,"counts":{"unknown":1}}`, at),
			fmt.Sprintf(`{"time":%d,"counts":{"uploads_ok":1e999}}`, at),
			fmt.Sprintf(`{"time":%d,"occupancy":-1}`, at),
			fmt.Sprintf(`{"time":%d,"checks":1,"reachable":2}`, at),
			fmt.Sprintf(`{"time":%d,"extra":{"gauges":{"go_memory_used_bytes":-1}}}`, at),
			fmt.Sprintf(`{"time":%d,"extra":{"histograms":{"upload":{"bounds":[0.1],"buckets":[1,1],"count":1}}}}`, at),
		} {
			st := newFibreStore()
			st.now = func() time.Time { return now }
			raw := fmt.Sprintf(`{"bad":{"%d":%s},"Chain":{"%d":{"time":%d,"counts":{"uploads_ok":2}}}}`, at, value, at, at)
			st.restoreShort(json.RawMessage(raw), span)
			if len(st.short[span]["bad"]) != 0 || len(st.short[span]["Chain"]) != 1 {
				t.Fatalf("%s accepted invalid entry or lost valid entry: %s", span, raw)
			}
		}
		for _, invalid := range []int64{at + 1, at + int64(step/time.Second), at - int64(duration/time.Second)} {
			st := newFibreStore()
			st.now = func() time.Time { return now }
			st.restoreShort(json.RawMessage(fmt.Sprintf(`{"Chain":{"%d":{"time":%d}}}`, invalid, invalid)), span)
			if len(st.short[span]) != 0 {
				t.Fatalf("%s restored unaligned, future or expired timestamp %d", span, invalid)
			}
		}
	}
}

func TestFibreShortRestartKeepsFirstExportAsBaseline(t *testing.T) {
	r := newFibreRig(t)
	r.cc.Alerts.FibreInstance = "fibre-host-1"
	f := r.feed(time.Hour)
	f.signFast = 100
	f.send()
	r.advance(time.Minute)
	f.signFast += 5
	f.send()
	fresh := newFibreStore()
	fresh.now = r.clock.Now
	fresh.restoreShort(r.st.savedShort("1h"), "1h")
	fresh.restoreShort(r.st.savedShort("24h"), "24h")
	r.c.fibreStore = fresh
	r.advance(time.Minute)
	f.signFast += 100
	f.send()
	if got := fresh.snapshot("Chain").Window.signOK; got != 0 {
		t.Fatalf("first cumulative export counted %d", got)
	}
	r.advance(time.Minute)
	f.signFast += 3
	f.send()
	if got := fresh.snapshot("Chain").Window.signOK; got != 3 {
		t.Fatalf("next export counted %d, want 3", got)
	}
}

func TestFibreShortLiveMergesPartialBucketOnce(t *testing.T) {
	st := newFibreStore()
	clock := &fibreClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	st.now = clock.Now
	st.senders["old"] = &fibreSender{chain: "Chain", lastReceived: clock.now,
		window: []fibreDelta{{at: clock.now, uploadOK: 2, known: 1, occupancy: 100, haveOccupancy: true}}}
	fresh := newFibreStore()
	fresh.now = clock.Now
	for _, span := range []string{"1h", "24h"} {
		fresh.restoreShort(st.savedShort(span), span)
	}
	clock.now = clock.now.Add(20 * time.Second)
	fresh.senders["new"] = &fibreSender{chain: "Chain", lastReceived: clock.now,
		window: []fibreDelta{{at: clock.now, uploadOK: 3, known: 1, occupancy: 80, haveOccupancy: true}}}
	for _, span := range []string{"1h", "24h"} {
		points := fresh.series("Chain", span)
		last := points[len(points)-1]
		if last.Metrics["uploads_ok"] != 5 || last.Occupancy == nil || *last.Occupancy != 80 {
			t.Fatalf("%s failed to merge partial bucket: %+v", span, last)
		}
		for i := 0; i < 2; i++ {
			copy := newFibreStore()
			copy.now = clock.Now
			copy.restoreShort(fresh.savedShort(span), span)
			if !reflect.DeepEqual(points, copy.series("Chain", span)) {
				t.Fatalf("%s double counted a repeated save", span)
			}
		}
	}
}

func TestFibreShortConfigRestoreDropsUnwatchedChains(t *testing.T) {
	oldAlarms := alarms
	t.Cleanup(func() { alarms = oldAlarms })
	dir := t.TempDir()
	configPath, statePath := filepath.Join(dir, "config.yml"), filepath.Join(dir, "state.json")
	data := "chains:\n  watched:\n    chain_id: demo\n    alerts:\n      fibre_enabled: yes\n  disabled:\n    chain_id: demo\n    alerts:\n      fibre_enabled: no\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	saved := savedState{}
	for _, span := range []string{"1h", "24h"} {
		_, step := fibreRange(span)
		at := time.Now().Truncate(step).Unix()
		raw := json.RawMessage(fmt.Sprintf(`{"watched":{"%d":{"time":%d}},"disabled":{"%d":{"time":%d}},"removed":{"%d":{"time":%d}}}`, at, at, at, at, at, at))
		if span == "1h" {
			saved.FibreMinutes = raw
		} else {
			saved.FibreTenmin = raw
		}
	}
	body, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, body, 0600); err != nil {
		t.Fatal(err)
	}
	password := ""
	loaded, err := loadConfigOptions(configPath, statePath, filepath.Join(dir, "chains.d"), &password, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.cancel()
	defer loaded.deliveryCancel()
	for _, span := range []string{"1h", "24h"} {
		chains := loaded.fibreData().short[span]
		if len(chains) != 1 || len(chains["watched"]) != 1 {
			t.Fatalf("%s retained unwatched chains: %+v", span, chains)
		}
	}
}

func TestFibreShortRestartDoesNotFillLoneDowntimeMinute(t *testing.T) {
	clock := &fibreClock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	st := newFibreStore()
	st.now = clock.Now
	st.recordReach("Chain", fibreState{Reachable: true})
	fresh := newFibreStore()
	fresh.now = clock.Now
	fresh.restoreShort(st.savedShort("1h"), "1h")
	clock.now = clock.now.Add(2 * time.Minute)
	fresh.recordReach("Chain", fibreState{Reachable: true})
	points := fresh.series("Chain", "1h")
	if points[len(points)-2].Reachable != nil {
		t.Fatal("filled a minute of downtime between restored and live checks")
	}
}
