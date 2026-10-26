package txn

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestInitialConfigDoesNotReplaceConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	manager := &Manager{LiveConfigPath: path, PendingPath: filepath.Join(root, "pending.json"), ConfigInstallOnly: true, ConfigMode: 0600, Logger: log.New(io.Discard, "", 0)}
	committed, err := manager.Run(context.Background(), "connect-1", KindConfig, Candidate{Config: []byte("ours")}, func(context.Context, ProbeInfo) error { return os.WriteFile(path, []byte("other writer"), 0600) })
	if err == nil || committed {
		t.Fatal("concurrent config overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "other writer" {
		t.Fatal("concurrent config lost")
	}
	if _, err := os.Stat(manager.PendingPath); !os.IsNotExist(err) {
		t.Fatal("stale pending marker")
	}
}
func TestInitialConfigPrivateAndExistingProtected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	manager := &Manager{LiveConfigPath: path, PendingPath: filepath.Join(root, "pending.json"), ConfigInstallOnly: true, ConfigMode: 0600, Logger: log.New(io.Discard, "", 0)}
	committed, err := manager.Run(context.Background(), "connect-1", KindConfig, Candidate{Config: []byte("ours")}, func(context.Context, ProbeInfo) error { return nil })
	if err != nil || !committed {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration is not private")
	}
	if done, err := manager.Run(context.Background(), "connect-2", KindConfig, Candidate{Config: []byte("new")}, func(context.Context, ProbeInfo) error { return nil }); err == nil || done {
		t.Fatal("existing config replaced")
	}
}
func TestInitialInstallRecoveryPreservesUnrelatedFile(t *testing.T) {
	for _, same := range []bool{false, true} {
		root := t.TempDir()
		path := filepath.Join(root, "config.yaml")
		staged := path + stagingSuffix
		os.WriteFile(staged, []byte("ours"), 0600)
		if same {
			os.Link(staged, path)
		} else {
			os.WriteFile(path, []byte("other"), 0600)
		}
		manager := &Manager{LiveConfigPath: path, PendingPath: filepath.Join(root, "pending.json"), Logger: log.New(io.Discard, "", 0)}
		if err := writePending(manager.PendingPath, &Pending{TxnID: "connect-1", Kind: KindConfig, State: StateInflight, NoPriorLiveConfig: true, ConfigInstallOnly: true}); err != nil {
			t.Fatal(err)
		}
		if err := manager.RecoverOnBoot(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if same && !os.IsNotExist(err) {
			t.Fatal("uncommitted initial config survived")
		}
		if !same && (err != nil || string(data) != "other") {
			t.Fatal("unrelated live config removed")
		}
	}
}
