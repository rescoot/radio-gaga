package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckAndStoreDBCFlavorPreservesKnownInformationOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		timeout bool
	}{
		{name: "ssh failure", script: "exit 255"},
		{name: "ssh timeout", script: "exec sleep 30", timeout: true},
		{name: "missing ID", script: "echo VERSION_ID=new-version"},
		{name: "missing version", script: "echo ID=librescoot"},
		{name: "empty response", script: "exit 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, server := newTestClient(t)
			server.HSet("system", "dbc-flavor", "librescoot", "dbc-version", "nightly-known", "dbc-sn", "known-serial")
			installTestSSH(t, tc.script)
			if tc.timeout {
				ctx, cancel := context.WithTimeout(s.ctx, 100*time.Millisecond)
				defer cancel()
				s.ctx = ctx
			}

			s.checkAndStoreDBCFlavor()

			for field, want := range map[string]string{
				"dbc-flavor": "librescoot", "dbc-version": "nightly-known", "dbc-sn": "known-serial",
			} {
				if got := server.HGet("system", field); got != want {
					t.Errorf("system[%s] = %q, want %q", field, got, want)
				}
			}
			if server.Exists("version:dbc") {
				t.Error("failed SSH lookup populated version:dbc")
			}
		})
	}
}

func TestCheckAndStoreDBCFlavorStoresSuccessfulLookup(t *testing.T) {
	for _, source := range []string{"redis", "ssh"} {
		t.Run(source, func(t *testing.T) {
			s, server := newTestClient(t)
			server.HSet("system", "dbc-flavor", "stock", "dbc-version", "old-version")
			if source == "redis" {
				server.HSet("version:dbc", "id", "librescoot", "version_id", "new-version")
				installTestSSH(t, "exit 255")
			} else {
				installTestSSH(t, "printf 'ID=librescoot\\nVERSION_ID=new-version\\n'")
			}

			s.checkAndStoreDBCFlavor()

			if got := server.HGet("system", "dbc-flavor"); got != "librescoot" {
				t.Errorf("flavor = %q, want librescoot", got)
			}
			if got := server.HGet("system", "dbc-version"); got != "new-version" {
				t.Errorf("version = %q, want new-version", got)
			}
			if got := server.HGet("version:dbc", "version_id"); got != "new-version" {
				t.Errorf("cached version = %q, want new-version", got)
			}
		})
	}
}

func installTestSSH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
