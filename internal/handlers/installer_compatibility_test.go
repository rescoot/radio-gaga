package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"radio-gaga/internal/installercompat"
	"radio-gaga/internal/txn"
)

func TestManagedSelfUpdateRequiresInstallerCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, report string
		valid        bool
	}{
		{"compatible", installercompat.Report, true},
		{"legacy", `{"enrollment_protocol":1,"installation_binding":1}`, false},
		{"invalid", "not JSON", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.report+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			called := false
			probe := managedInstallerProbe(func(context.Context, txn.ProbeInfo) error { called = true; return nil })
			err := probe(context.Background(), txn.ProbeInfo{BinaryPath: binary, ConfigPath: "not-read"})
			if (err == nil) != tc.valid || called != tc.valid {
				t.Fatalf("valid=%v called=%v err=%v", tc.valid, called, err)
			}
		})
	}
}
