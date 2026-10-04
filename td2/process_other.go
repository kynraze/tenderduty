//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package tenderduty

import (
	"fmt"
	"os"
	"strconv"
)

func reloadSignal() os.Signal {
	return nil
}

func sendReload(pid int) error {
	return fmt.Errorf("reload needs Linux/macOS/Docker")
}

// No Unix signals here; exclusive PID file only.
func acquirePID(stateFile string) (func(), error) {
	filename := stateFile + ".pid"
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot claim %s (remove a stale PID file before starting): %w", filename, err)
	}
	_, err = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(filename)
		if err != nil {
			return nil, err
		}
		return nil, closeErr
	}
	return func() { _ = os.Remove(filename) }, nil
}
