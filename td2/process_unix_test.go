//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package tenderduty

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReloadSentAfterChainEdit(t *testing.T) {
	options, _, _ := chainCommandFixture(t)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	defer signal.Stop(signals)
	release, err := acquirePID(options.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "extra_info=updated"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals:
	case <-time.After(2 * time.Second):
		t.Fatal("chain edit did not send SIGHUP")
	}
	if !strings.Contains(output.String(), fmt.Sprintf("Applied: reloaded tenderduty (pid %d)", os.Getpid())) {
		t.Fatalf("missing PID feedback: %s", output.String())
	}
}

func TestReloadSaysWhoWasReloaded(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	defer signal.Stop(signals)
	release, err := acquirePID(state)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var output bytes.Buffer
	if err := Reload(state, &output); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("Reloaded tenderduty (pid %d)\n", os.Getpid()); output.String() != want {
		t.Fatalf("got %q, want %q", output.String(), want)
	}
	select {
	case <-signals:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not send SIGHUP")
	}
}

func TestPIDLiveProcessWithoutLockReplaced(t *testing.T) {
	// without a held lock, even a live stored PID is stale
	for name, pid := range map[string]int{"parent": os.Getppid(), "own": os.Getpid()} {
		state := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(pid)), 0600); err != nil {
			t.Fatal(err)
		}
		release, err := acquirePID(state)
		if err != nil {
			t.Fatalf("%s PID without a lock blocked startup: %v", name, err)
		}
		got, err := readPID(state)
		release()
		if err != nil || got != os.Getpid() {
			t.Fatalf("%s PID not replaced: %d, %v", name, got, err)
		}
	}
}

func TestPIDFileSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state.json")
	if err := os.Symlink(target, state+".pid"); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if release, err := acquirePID(state); err == nil {
		release()
		t.Fatal("a symlinked PID file was followed")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("the symlink target was written: %q", data)
	}
}

func TestReloadRefusesReusedPID(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Skipf("sleep is unavailable: %v", err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(command.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Reload(state, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("unrelated process accepted: %v", err)
	}
	release, err := acquirePID(state)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
