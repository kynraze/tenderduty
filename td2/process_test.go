package tenderduty

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPIDLifecycleAndSecondInstance(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	release, err := acquirePID(state)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	pid, err := readPID(state)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("PID=%d, error=%v", pid, err)
	}
	if second, err := acquirePID(state); err == nil {
		second()
		t.Fatal("second instance accepted")
	}
	release()
	if _, err := os.Stat(state + ".pid"); !os.IsNotExist(err) {
		t.Fatalf("PID file not removed: %v", err)
	}
}

func TestPIDStaleReplaced(t *testing.T) {
	if reloadSignal() == nil {
		// Windows supports replacement; other unsupported OSes are conservative.
		if os.PathSeparator != '\\' {
			t.Skip("PID replacement requires Unix or Windows")
		}
	}
	for _, stale := range []string{"invalid", "-1", "2147483647"} {
		state := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(state+".pid", []byte(stale), 0600); err != nil {
			t.Fatal(err)
		}
		release, err := acquirePID(state)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := readPID(state)
		release()
		if err != nil || pid != os.Getpid() {
			t.Fatalf("stale %q not replaced: %d, %v", stale, pid, err)
		}
	}
}

func TestReloadMissingStaleAndUnsupported(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	var output bytes.Buffer
	if reloadSignal() == nil {
		if err := Reload(state, &output); err == nil || !strings.Contains(err.Error(), "Linux/macOS/Docker") {
			t.Fatalf("unsupported reload: %v", err)
		}
		return
	}
	if err := Reload(state, &output); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("missing PID: %v", err)
	}
	for _, stale := range []string{"invalid", "0", "-1", strconv.Itoa(2147483647)} {
		if err := os.WriteFile(state+".pid", []byte(stale), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Reload(state, &output); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("stale PID %s: %v", stale, err)
		}
	}
}

func TestStartupFailureRemovesPID(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	password := ""
	old := td
	defer func() { td = old }()
	if err := runContext(context.Background(), filepath.Join(dir, "missing.yml"), state, filepath.Join(dir, "chains.d"), &password); err == nil {
		t.Fatal("missing config accepted")
	}
	if _, err := os.Stat(state + ".pid"); !os.IsNotExist(err) {
		t.Fatalf("startup failure left a PID file: %v", err)
	}
}
