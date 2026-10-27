package txn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateStagingUsesFreshInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml.staging")
	if err := os.WriteFile(path, []byte("old-public-data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := writeFileSync(path, []byte("private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("staging permissions do not protect the credential")
	}
	data := make([]byte, 64)
	n, err := old.Read(data)
	if err != nil || string(data[:n]) != "old-public-data" {
		t.Fatal("an existing reader can observe the replacement credential")
	}
}

func TestStagingDoesNotFollowOldSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unrelated")
	path := filepath.Join(dir, "config.yaml.staging")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := writeFileSync(path, []byte("private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("staging changed the symlink target")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatal("staging is not a private regular file")
	}
}
