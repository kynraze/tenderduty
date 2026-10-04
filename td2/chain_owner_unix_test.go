//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package tenderduty

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func ownerOf(t *testing.T, path string) (os.FileMode, int, int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return info.Mode().Perm(), int(stat.Uid), int(stat.Gid)
}

func TestNewChainFileFollowsTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		directory os.FileMode
		want      os.FileMode
	}{{0750, 0640}, {0770, 0640}, {0700, 0600}, {0705, 0600}} {
		dir := filepath.Join(t.TempDir(), "chains.d")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, tc.directory); err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(dir, "New.yml")
		if err := writeChainFile(filename, []byte("chain_id: a\n"), 0600, false); err != nil {
			t.Fatal(err)
		}
		mode, _, gid := ownerOf(t, filename)
		_, _, dirGID := ownerOf(t, dir)
		if mode != tc.want || gid != dirGID {
			t.Fatalf("directory %o: file mode %o group %d, want %o group %d", tc.directory, mode, gid, tc.want, dirGID)
		}
	}
}

func TestReplacedChainFileKeepsItsModeAndOwner(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "Old.yml")
	if err := os.WriteFile(filename, []byte("chain_id: a\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, 0640); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(filename, 1234, 4321); err != nil {
			t.Fatal(err)
		}
	}
	_, uid, gid := ownerOf(t, filename)
	// callers pass 0600 on --force; the file keeps what it had
	if err := writeChainFile(filename, []byte("chain_id: b\n"), 0600, true); err != nil {
		t.Fatal(err)
	}
	mode, newUID, newGID := ownerOf(t, filename)
	if mode != 0640 || newUID != uid || newGID != gid {
		t.Fatalf("mode %o owner %d:%d, want 640 %d:%d", mode, newUID, newGID, uid, gid)
	}
	if data, _ := os.ReadFile(filename); string(data) != "chain_id: b\n" {
		t.Fatalf("contents = %q", data)
	}
}

func TestChainSetAsRootKeepsTheServiceOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving files to another user needs root")
	}
	options, filename, _ := chainCommandFixture(t)
	if err := os.Chown(filename, 1234, 4321); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, 0640); err != nil {
		t.Fatal(err)
	}
	options.reload = func(string) (int, error) { return 1, nil }
	if err := ChainCommand(context.Background(), options, []string{"set", "Celestia", "extra_info=changed"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if mode, uid, gid := ownerOf(t, filename); mode != 0640 || uid != 1234 || gid != 4321 {
		t.Fatalf("mode %o owner %d:%d", mode, uid, gid)
	}
	// a new file takes the directory's group
	if err := os.Chown(options.Directory, 0, 4321); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(options.Directory, 0750); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(options.Directory, "New.yml")
	if err := writeChainFile(created, []byte("chain_id: a\n"), 0600, false); err != nil {
		t.Fatal(err)
	}
	if mode, _, gid := ownerOf(t, created); mode != 0640 || gid != 4321 {
		t.Fatalf("new file mode %o group %d", mode, gid)
	}
	// and so does a chain directory the command creates
	parent := t.TempDir()
	if err := os.Chown(parent, 0, 4321); err != nil {
		t.Fatal(err)
	}
	if err := makeChainDirectory(filepath.Join(parent, "chains.d")); err != nil {
		t.Fatal(err)
	}
	if mode, _, gid := ownerOf(t, filepath.Join(parent, "chains.d")); mode != 0750 || gid != 4321 {
		t.Fatalf("directory mode %o group %d", mode, gid)
	}
}

func TestMakeChainDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "chains.d")
	if err := makeChainDirectory(dir); err != nil {
		t.Fatal(err)
	}
	mode, _, gid := ownerOf(t, dir)
	_, _, parentGID := ownerOf(t, parent)
	if mode != 0750 || gid != parentGID {
		t.Fatalf("directory mode %o group %d, want 750 group %d", mode, gid, parentGID)
	}
	// an existing directory is left as it is
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := makeChainDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if mode, _, _ := ownerOf(t, dir); mode != 0700 {
		t.Fatalf("existing directory changed to %o", mode)
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := makeChainDirectory(file); err == nil {
		t.Fatal("a file was taken for the chain directory")
	}
}
