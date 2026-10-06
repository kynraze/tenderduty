//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package tenderduty

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// chainFileOwner returns the file mode and owner; -1 leaves an ID unchanged.
// Replacements keep their owner and mode; new files inherit the directory group and read access.
func chainFileOwner(filename string, replace bool, mode os.FileMode) (os.FileMode, int, int) {
	if replace {
		if info, err := os.Stat(filename); err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				return info.Mode().Perm(), int(stat.Uid), int(stat.Gid)
			}
		}
	}
	info, err := os.Stat(filepath.Dir(filename))
	if err != nil {
		return mode, -1, -1
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode, -1, -1
	}
	if info.Mode().Perm()&0050 == 0050 {
		return 0640, -1, int(stat.Gid)
	}
	return 0600, -1, int(stat.Gid)
}

// chownTemp ignores EPERM when the caller cannot change ownership.
func chownTemp(file *os.File, uid, gid int) error {
	if uid < 0 && gid < 0 {
		return nil
	}
	if err := file.Chown(uid, gid); err != nil && !errors.Is(err, syscall.EPERM) {
		return err
	}
	return nil
}

// makeChainDirectory creates a missing directory as 0750 with its parent group.
func makeChainDirectory(directory string) error {
	_, statErr := os.Stat(directory)
	if err := os.MkdirAll(directory, 0750); err != nil || statErr == nil {
		return err
	}
	if err := os.Chmod(directory, 0750); err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(directory)))
	if err != nil {
		return nil
	}
	if stat, ok := parent.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(directory, -1, int(stat.Gid)); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	return nil
}
