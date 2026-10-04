package tenderduty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// notRunningError means no PID file exists for the state file.
type notRunningError struct{ stateFile string }

func (e *notRunningError) Error() string {
	return fmt.Sprintf("tenderduty is not running (no process for state file %s); start it, or pass the -state file it uses", e.stateFile)
}

var errReloadUnsupported = errors.New("reload needs Linux/macOS/Docker")

func readPID(stateFile string) (int, error) {
	data, err := os.ReadFile(stateFile + ".pid")
	if os.IsNotExist(err) {
		return 0, &notRunningError{stateFile}
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("stale or invalid PID file %s.pid", stateFile)
	}
	return pid, nil
}

// reloadProcess signals the running tenderduty and returns its pid.
func reloadProcess(stateFile string) (int, error) {
	if reloadSignal() == nil {
		return 0, errReloadUnsupported
	}
	pid, err := readPID(stateFile)
	if err != nil {
		return 0, err
	}
	if err := sendReload(pid); err != nil {
		return 0, fmt.Errorf("could not reload pid %d: %w", pid, err)
	}
	return pid, nil
}

func Reload(stateFile string, output io.Writer) error {
	pid, err := reloadProcess(stateFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Reloaded tenderduty (pid %d)\n", pid)
	return nil
}

// reloadAfterEdit says whether a saved change is live; reload is replaceable for tests.
func reloadAfterEdit(stateFile string, output io.Writer, reload func(string) (int, error)) {
	if reload == nil {
		reload = reloadProcess
	}
	pid, err := reload(stateFile)
	var notRunning *notRunningError
	switch {
	case err == nil:
		fmt.Fprintf(output, "Applied: reloaded tenderduty (pid %d)\n", pid)
	case errors.As(err, &notRunning):
		fmt.Fprintln(output, "Not applied yet: tenderduty is not running; it loads the change when it starts")
	case errors.Is(err, errReloadUnsupported):
		fmt.Fprintln(output, "Not applied yet: reload needs Linux/macOS/Docker; restart tenderduty to load the change")
	default:
		fmt.Fprintf(output, "Saved, but reload failed: %v; run tenderduty reload\n", err)
	}
}
