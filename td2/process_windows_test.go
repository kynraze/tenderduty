//go:build windows

package tenderduty

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPIDOwnProcessWithoutLockReplaced(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state+".pid", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	release, err := acquirePID(state)
	if err != nil {
		t.Fatalf("our own stale PID blocked startup: %v", err)
	}
	release()
}
