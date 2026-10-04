//go:build windows

package tenderduty

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

func reloadSignal() os.Signal {
	return nil
}
func sendReload(pid int) error {
	return fmt.Errorf("reload needs Linux/macOS/Docker")
}

func processAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return true
	}
	if code != 259 {
		return false
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		return true
	}
	return strings.EqualFold(filepath.Base(windows.UTF16ToString(buffer[:size])), filepath.Base(executable))
}

func acquirePID(stateFile string) (func(), error) {
	filename := stateFile + ".pid"
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		_ = file.Close()
		return nil, err
	}
	// Lock beyond the contents so reload/status readers can still read the PID.
	overlap := &windows.Overlapped{OffsetHigh: 1}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap); err != nil {
		return fail(fmt.Errorf("tenderduty is already running on state file %s: %w", stateFile, err))
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return fail(err)
	}
	// a stored PID can be this process again after a crash and restart
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && pid != os.Getpid() && processAlive(pid) {
		return fail(fmt.Errorf("tenderduty is already running as %d on state file %s", pid, stateFile))
	}
	if err := file.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	return func() {
		_ = file.Close()
		_ = os.Remove(filename)
	}, nil
}
