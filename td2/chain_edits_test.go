package tenderduty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
)

func TestChainSetMany(t *testing.T) {
	for _, args := range [][]string{
		{"set", "celestia", "Other", "public_fallback=on"},
		{"set", "--all", "public_fallback=on"},
		{"set", "celestia", "public_fallback=on", "Celestia", "Other"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			other := filepath.Join(options.Directory, "Other.yml")
			if err := os.WriteFile(other, bytes.ReplaceAll(original, []byte("celestiavaloper1test"), []byte("othervaloper1test")), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			options.reload = func(string) (int, error) { calls++; return 123, nil }
			var output bytes.Buffer
			if err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{filename, other} {
				data, _ := os.ReadFile(file)
				if !bytes.Contains(data, []byte("public_fallback: yes")) {
					t.Fatalf("unchanged %s: %s", file, data)
				}
			}
			if calls != 1 || !strings.Contains(output.String(), "Updated 2 chain files") || strings.Count(output.String(), "Celestia: public_fallback:") != 1 {
				t.Fatalf("reloads %d, output %s", calls, &output)
			}
			if err := ChainCommand(context.Background(), options, args, strings.NewReader(""), &output); err != nil || calls != 1 {
				t.Fatalf("no-op reloaded: %v, %d", err, calls)
			}
		})
	}
}

func TestChainSetManyRefusals(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"set", "Celestia", "missing", "public_fallback=yes"}, "no chain"},
		{[]string{"set", "--all", "Celestia", "public_fallback=yes"}, "set needs"},
		{[]string{"set", "Celestia", "public_fallback"}, "expected key=value"},
		{[]string{"set", "Celestia", "alerts.telegram.min_severity=warn"}, "must be info, warning, error or critical"},
		{[]string{"remove", "Cel", "--yes"}, "no chain"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			err := ChainCommand(context.Background(), options, tc.args, strings.NewReader(""), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			data, _ := os.ReadFile(filename)
			if !bytes.Equal(data, original) {
				t.Fatal("failed command wrote the file")
			}
		})
	}
}

func TestChainNameAmbiguity(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	other := filepath.Join(options.Directory, "celestia.yml")
	if err := os.WriteFile(other, original, 0600); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(options.Directory)
	if len(entries) != 2 {
		t.Skip("filesystem is case-insensitive")
	}
	err := ChainCommand(context.Background(), options, []string{"set", "CELESTIA", "public_fallback=yes"}, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `"CELESTIA" matches Celestia, celestia; use the exact name`) {
		t.Fatalf("ambiguous name accepted: %v", err)
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Equal(data, original) {
		t.Fatal("ambiguous name wrote the file")
	}
}

func TestChainAddCaseCollision(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	add := AddChainOptions{Valoper: "newvaloper1test", Node: "https://rpc.example", Name: "celestia", Directory: options.Directory}
	err := addChain(context.Background(), add, &bytes.Buffer{}, addChainTestFetchers())
	if err == nil || !strings.Contains(err.Error(), "Celestia already exists as") {
		t.Fatalf("case collision accepted: %v", err)
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Equal(data, original) {
		t.Fatal("collision overwrote file")
	}
	add.Force = true
	if err := addChain(context.Background(), add, &bytes.Buffer{}, addChainTestFetchers()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(options.Directory)
	if len(entries) != 1 || entries[0].Name() != "Celestia.yml" {
		t.Fatal("force created a second file")
	}
}

func TestChainRenameCLI(t *testing.T) {
	for _, next := range []string{"NewName", "celestia"} {
		t.Run(next, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			calls := 0
			options.reload = func(string) (int, error) { calls++; return 123, nil }
			var output bytes.Buffer
			if err := ChainCommand(context.Background(), options, []string{"rename", "CELESTIA", next}, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(options.Directory, next+".yml"))
			entries, _ := os.ReadDir(options.Directory)
			if err != nil || !bytes.Equal(data, original) || calls != 1 || len(entries) != 1 || entries[0].Name() != next+".yml" {
				t.Fatalf("rename failed: %v, %s, %s", err, filename, &output)
			}
		})
	}
	options, filename, original := chainCommandFixture(t)
	if err := os.WriteFile(filepath.Join(options.Directory, "Other.yml"), original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := renameChainFile(options, "Celestia", "OTHER", &bytes.Buffer{}); err == nil {
		t.Fatal("overwrote target")
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Equal(data, original) {
		t.Fatal("rename changed source")
	}
}

func TestChainNodes(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	var chainID atomic.Value
	chainID.Store("chain-1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID interface{} `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"node_info":{"network":%q},"sync_info":{"catching_up":false}}}`, stringJSON(request.ID), chainID.Load().(string))
	}))
	defer server.Close()
	options.reload = func(string) (int, error) { return 123, nil }
	command := func(args ...string) error {
		return ChainCommand(context.Background(), options, args, strings.NewReader(""), &bytes.Buffer{})
	}
	if err := command("node", "remove", "celestia", "https://rpc.example/"); err == nil || !strings.Contains(err.Error(), "has only this node") {
		t.Fatalf("removed last node: %v", err)
	}
	chainID.Store("wrong")
	if err := command("node", "add", "celestia", server.URL); err == nil || !strings.Contains(err.Error(), "node reports chain id wrong") {
		t.Fatalf("accepted wrong chain: %v", err)
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Equal(data, original) {
		t.Fatal("failed node edit changed file")
	}
	chainID.Store("chain-1")
	if err := command("node", "add", "celestia", server.URL, "--alert", "maybe"); err == nil {
		t.Fatal("accepted --alert maybe")
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"node", "add", "celestia", server.URL}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filename)
	if !bytes.Contains(data, []byte("# Keep this comment")) || !bytes.Contains(data, []byte("alert_if_down: no")) || !strings.Contains(output.String(), "alert_if_down: no") {
		t.Fatalf("lost comments or a second node alerts by default: %s\n%s", data, &output)
	}
	if err := command("node", "add", "Celestia", server.URL+"/"); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := command("node", "remove", "celestia", server.URL+"/"); err != nil {
		t.Fatal(err)
	}
}

func TestChainLeadingHome(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	var output bytes.Buffer
	args := []string{"-home", filepath.Dir(options.ConfigFile), "list"}
	if err := ChainCommand(context.Background(), ChainCommandOptions{}, args, strings.NewReader(""), &output); err != nil || !strings.Contains(output.String(), "Celestia") {
		t.Fatalf("leading flag failed: %v, %s", err, &output)
	}
}

func TestRenameRuntimeState(t *testing.T) {
	for _, old := range []string{"Old", "Old / A"} {
		t.Run(old, func(t *testing.T) {
			c := auditConfig(t)
			// Cancel before starting replacement workers; no network is needed.
			c.cancel()
			nextName := strings.Replace(old, "Old", "New", 1)
			before, after := reloadTestChain(), reloadTestChain()
			before.name, after.name = old, nextName
			c.Chains[old] = before
			c.updateChan = make(chan *dash.ChainStatus, 8)
			c.statsChan = make(chan *promUpdate, 8)
			now := time.Now()
			message := "active alarm"
			alarms.AllAlarms[old] = map[string]time.Time{message: now}
			alarms.AlarmSeverities = map[string]string{old + "\x00" + message: "critical"}
			c.reportData.Hours = map[string][]reportHour{old: {{}}}
			c.fibreData().hours[old] = map[int64]*fibreHour{1: {Time: 1}}
			c.bridgeData().tiers[old] = map[string]map[int64]bridgeBucket{"1h": {1: {Time: 1}}}
			diff := c.applyReload(&Config{Chains: map[string]*ChainConfig{nextName: after}})
			stopChain(after)
			if diff.Renamed[old] != nextName || len(diff.Removed)+len(diff.Added) != 0 {
				t.Fatalf("rename not paired: %+v", diff)
			}
			if !alarmIsActive(nextName, message) || alarmIsActive(old, message) || len(alarms.Outbox) != 0 {
				t.Fatal("alarm lost or recovery queued")
			}
			if len(c.reportData.Hours[nextName]) != 1 || len(c.fibreData().hours[nextName]) != 1 || len(c.bridgeData().tiers[nextName]) != 1 {
				t.Fatal("history did not move")
			}
		})
	}
}

func TestRenamePairRequiresUniqueIdentity(t *testing.T) {
	old := map[string]*ChainConfig{"Old": reloadTestChain()}
	next := map[string]*ChainConfig{"New": reloadTestChain()}
	next["New"].ChainId = "different"
	diff := diffChains(old, next)
	diff.pairRenames(old, next)
	if len(diff.Renamed) != 0 || len(diff.Added) != 1 || len(diff.Removed) != 1 {
		t.Fatal("different chains paired")
	}
	next["New"].ChainId = old["Old"].ChainId
	next["Another"] = reloadTestChain()
	diff = diffChains(old, next)
	diff.pairRenames(old, next)
	if len(diff.Renamed) != 0 {
		t.Fatal("ambiguous validators paired")
	}
}

func stringJSON(value interface{}) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestChainBooleanOnOff(t *testing.T) {
	options, filename, _ := chainCommandFixture(t)
	options.reload = func(string) (int, error) { return 123, nil }
	for _, value := range []string{"ON", "off"} {
		if err := ChainCommand(context.Background(), options, []string{"set", "celestia", "public_fallback=" + value}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filename)
	if !bytes.Contains(data, []byte("public_fallback: no")) {
		t.Fatalf("off not applied: %s", data)
	}
}
