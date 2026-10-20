//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package tenderduty

import "os"

// No file owners to keep here.
func chainFileOwner(_ string, _ bool, mode os.FileMode) (os.FileMode, int, int) {
	return mode, -1, -1
}

func chownTemp(*os.File, int, int) error { return nil }

func makeChainDirectory(directory string) error { return os.MkdirAll(directory, 0750) }
