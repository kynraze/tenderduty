//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package tenderduty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func reloadSignal() os.Signal {
	return syscall.SIGHUP
}

func acquirePID(stateFile string) (func(), error) {
	filename := stateFile + ".pid"
	// O_NOFOLLOW refuses a planted symlink
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("tenderduty is already running on state file %s: %w", stateFile, err))
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	current, err := os.Stat(filename)
	if err != nil || !os.SameFile(info, current) {
		return fail(fmt.Errorf("PID file changed during startup; retry"))
	}
	// PID reuse, including Docker PID 1, makes the lock necessary
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
		if current, err := os.Stat(filename); err == nil && os.SameFile(info, current) {
			_ = os.Remove(filename)
		}
		_ = file.Close()
	}, nil
}

func processIdentity(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false, nil
	}
	// Avoid sending HUP to an unrelated process after a stale PID is reused.
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	var process string
	if runtime.GOOS == "linux" {
		process, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		process = strings.TrimSuffix(process, " (deleted)")
	} else {
		var data []byte
		data, err = exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
		process = strings.TrimSpace(string(data))
	}
	if err != nil {
		return false, fmt.Errorf("could not verify process %d: %w", pid, err)
	}
	return filepath.Base(process) == filepath.Base(executable), nil
}

func sendReload(pid int) error {
	alive, err := processIdentity(pid)
	if err != nil {
		return err
	}
	if !alive {
		return fmt.Errorf("stale PID file, that process is not tenderduty")
	}
	return syscall.Kill(pid, syscall.SIGHUP)
}
