package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	td2 "github.com/kynraze/tenderduty/v2/td2"
)

// cliFixture is a config.yml and a chains.d with one chain.
func cliFixture(t *testing.T) (config, dir, state string) {
	t.Helper()
	root := t.TempDir()
	config, dir, state = filepath.Join(root, "config.yml"), filepath.Join(root, "chains.d"), filepath.Join(root, "state.json")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("alert_defaults:\n  upgrade_enabled: yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chain := "chain_id: chain-1\nvaloper_address: celestiavaloper1test\nnodes:\n  - url: https://rpc.example\n    alert_if_down: true\n"
	if err := os.WriteFile(filepath.Join(dir, "Celestia.yml"), []byte(chain), 0600); err != nil {
		t.Fatal(err)
	}
	return config, dir, state
}

func TestErrorsPrintWithoutTimestamp(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	missing := filepath.Join(t.TempDir(), "missing.yml")
	code, stdout, stderr := runCLI("-f", missing, "-cc", t.TempDir(), "chain", "list")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "Error: no missing.yml in ") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if regexp.MustCompile(`\d{4}/\d{2}/\d{2}|\d{2}:\d{2}:\d{2}`).MatchString(stderr) || logged.Len() != 0 {
		t.Fatalf("a timestamp or log line reached the operator: stderr %q, log %q", stderr, logged.String())
	}
	config, dir, state := cliFixture(t)
	code, _, stderr = runCLI("-f", config, "-cc", dir, "-state", state, "chain", "set", "Nope", "extra_info=x")
	want := "Error: no chain \"Nope\" in " + dir + " (chains: Celestia); see tenderduty chain list\n"
	if code != 1 || stderr != want || logged.Len() != 0 {
		t.Fatalf("exit %d, stderr %q, want %q", code, stderr, want)
	}
	code, _, stderr = runCLI("-f", config, "-cc", dir, "-state", state, "chain", "add", "celestiavaloper1test", "--node", "notaurl")
	if code != 1 || stderr != "Error: --node must be an http:// or https:// URL, got \"notaurl\"\n" {
		t.Fatalf("add: exit %d, stderr %q", code, stderr)
	}
}

func TestFinishPrintsOneErrorLine(t *testing.T) {
	var errs bytes.Buffer
	if code := finish(os.ErrNotExist, &errs); code != 1 || errs.String() != "Error: file does not exist\n" {
		t.Fatalf("exit %d, %q", code, errs.String())
	}
	errs.Reset()
	usage := &td2.UsageError{Err: os.ErrInvalid, Usage: "Usage:\n  tenderduty x\n"}
	if code := finish(usage, &errs); code != 1 || errs.String() != "Error: invalid argument\n\nUsage:\n  tenderduty x\n" {
		t.Fatalf("exit %d, %q", code, errs.String())
	}
	if code := finish(nil, &errs); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestReloadMessages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reload needs Linux/macOS/Docker")
	}
	state := filepath.Join(t.TempDir(), "state.json")
	code, stdout, stderr := runCLI("-state", state, "reload")
	want := "Error: tenderduty is not running (no process for state file " + state + "); start it, or pass the -state file it uses\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = runCLI("reload", "extra")
	if code != 1 || !strings.HasPrefix(stderr, "Error: reload takes no arguments, got 1: \"extra\"\n\nUsage:\n  tenderduty reload [flags]\n") {
		t.Fatalf("reload extra: exit %d, stderr %q", code, stderr)
	}
}

func TestChainListWithoutChainsExitsCleanly(t *testing.T) {
	config, _, state := cliFixture(t)
	code, stdout, stderr := runCLI("-f", config, "-cc", t.TempDir(), "-state", state, "chain", "list")
	if code != 0 || stderr != "" || stdout != "No chains yet. Add one: tenderduty chain add <valoper> --node <url>\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestBareChainPrintsTheHelp(t *testing.T) {
	help, _ := td2.ChainHelp(nil)
	for _, args := range [][]string{{"chain"}, {"help", "chain"}, {"chain", "-h"}, {"chain", "help"}} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 || stderr != "" || stdout != help {
			t.Fatalf("%v: exit %d, stderr %q, stdout %q", args, code, stderr, stdout)
		}
	}
	code, stdout, stderr := runCLI("chain", "bogus")
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "Error: unknown chain command \"bogus\"\n") {
		t.Fatalf("unknown subcommand: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestMissingConfigSaysWhereItLooked(t *testing.T) {
	empty := t.TempDir()
	t.Chdir(empty)
	t.Setenv(td2.HomeEnv, "")
	t.Setenv("CONFIG", "")
	system := filepath.Join(t.TempDir(), "etc")
	saved := td2.SystemConfigFile
	td2.SystemConfigFile = filepath.Join(system, "config.yml")
	defer func() { td2.SystemConfigFile = saved }()
	for _, args := range [][]string{nil, {"start"}} {
		code, _, stderr := runCLI(args...)
		want := "Error: no config.yml in " + empty + " or " + system + "\nCreate one: tenderduty -example-config > config.yml (or " + td2.SystemConfigFile + "), or pass -home or -f\n"
		if runtime.GOOS == "windows" {
			want = "Error: config.yml not found\nCreate one: tenderduty -example-config > config.yml\n"
		}
		if code != 1 || stderr != want {
			t.Fatalf("%v: exit %d, stderr %q, want %q", args, code, stderr, want)
		}
	}
	missing := filepath.Join(empty, "other.yml")
	code, _, stderr := runCLI("start", "-f", missing)
	if want := "Error: " + missing + " not found\nCreate one: tenderduty -example-config > " + missing + "\n"; code != 1 || stderr != want {
		t.Fatalf("-f after start: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCLI("start", "extra"); code != 1 || !strings.Contains(stderr, "start") {
		t.Fatalf("start with an argument: exit %d, stderr %q", code, stderr)
	}
}
