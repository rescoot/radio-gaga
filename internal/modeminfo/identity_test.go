package modeminfo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchIdentityUsesSelectedSIM(t *testing.T) {
	const script = `#!/bin/sh
case "$*" in
  "-J -L") printf '%s' '{"modem-list":["/org/freedesktop/ModemManager1/Modem/7"]}' ;;
  "-J -m /org/freedesktop/ModemManager1/Modem/7") printf '%s' '{"modem":{"generic":{"equipment-identifier":"866802020000001","sim":"/org/freedesktop/ModemManager1/SIM/9"}}}' ;;
  "-J -i /org/freedesktop/ModemManager1/SIM/9") printf '%s' '{"sim":{"properties":{"iccid":"8949000000000000001","imsi":"901280000000001"}}}' ;;
  *) exit 2 ;;
esac
`
	for _, tc := range []struct {
		name, old, replacement string
		fail                   bool
	}{
		{name: "active SIM"},
		{name: "missing IMSI", old: "901280000000001", replacement: "--"},
		{name: "invalid IMSI", old: "901280000000001", replacement: "abc", fail: true},
		{name: "missing SIM", old: "/org/freedesktop/ModemManager1/SIM/9", replacement: "--", fail: true},
		{name: "invalid IMEI", old: "866802020000001", replacement: "--", fail: true},
		{name: "invalid ICCID", old: "8949000000000000001", replacement: "123", fail: true},
		{name: "no modem", old: `"modem-list":["/org/freedesktop/ModemManager1/Modem/7"]`, replacement: `"modem-list":[]`, fail: true},
		{name: "multiple modems", old: `"modem-list":["/org/freedesktop/ModemManager1/Modem/7"]`, replacement: `"modem-list":["a","b"]`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			body := script
			if tc.old != "" {
				body = strings.ReplaceAll(body, tc.old, tc.replacement)
			}
			if err := os.WriteFile(filepath.Join(bin, "mmcli"), []byte(body), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			identity, err := FetchIdentity(context.Background())
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v, want failure=%v", err, tc.fail)
			}
			if !tc.fail {
				if identity.IMEI != "866802020000001" || identity.ICCID != "8949000000000000001" {
					t.Fatalf("incorrect identity: %+v", identity)
				}
				want := "901280000000001"
				if tc.name == "missing IMSI" {
					want = ""
				}
				if identity.IMSI != want {
					t.Fatalf("IMSI=%q, want %q", identity.IMSI, want)
				}
			}
		})
	}
}
