package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateAtomicState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "token.json")
	if _, err := ReadFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	for _, data := range []string{"first", "replacement"} {
		if err := WriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatalf("read = %q, %v", got, err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("token permissions: %v, %v", info, err)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("unexpected atomic-write leftovers: %v, %v", files, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err == nil {
		t.Fatal("insecure token permissions accepted")
	}
}

func TestPrivateDirectoryRejectsSymlinkAndPublicModes(t *testing.T) {
	parent := t.TempDir()
	public := filepath.Join(parent, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(public); err == nil {
		t.Fatal("public directory accepted")
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(link); err == nil {
		t.Fatal("symlink accepted as private directory")
	}
}

func TestLockContentionAndRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil || !strings.Contains(err.Error(), "another migration") {
		t.Fatalf("second lock = %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock, err = Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestLockRejectsSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("not a lock"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("lock followed a symlink")
	}
}

func TestFailedReplacementCleansOnlyOwnedTemporaryFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	target := filepath.Join(dir, "token.json")
	if err := EnsureDir(target); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("data")); err == nil {
		t.Fatal("replaced a directory with state file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "token.json" || !entries[0].IsDir() {
		t.Fatalf("unexpected cleanup contents: %v, %v", entries, err)
	}
}
