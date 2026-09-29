package modeminfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchDiscoversConcreteModemID(t *testing.T) {
	binDir := t.TempDir()
	mmcli := filepath.Join(binDir, "mmcli")
	script := `#!/bin/sh
case "$*" in
  "-J -L")
    printf '%s\n' '{"modem-list":["/org/freedesktop/ModemManager1/Modem/7"]}'
    ;;
  "-J -m 7")
    printf '%s\n' '{"modem":{"generic":{"manufacturer":"SIMCOM","model":"SIM7100E","hardware-revision":"1","firmware-revision":"baseband","device-identifier":"device","equipment-identifier":"123456789012345","own-numbers":["+49123"],"supported-modes":["4g"],"current-modes":"4g"}}}'
    ;;
  "-m 7 --command=AT+SIMCOMATI")
    printf '%s\n' 'response: |' '  Revision: LE11B01SIM7100E'
    ;;
  *)
    echo "unexpected arguments: $*" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(mmcli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	info, err := Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if info.EquipmentID != "123456789012345" {
		t.Errorf("EquipmentID = %q", info.EquipmentID)
	}
	if info.VendorFirmwareRevision != "LE11B01SIM7100E" {
		t.Errorf("VendorFirmwareRevision = %q", info.VendorFirmwareRevision)
	}
}

func TestDiscoverModemIDRejectsEmptyList(t *testing.T) {
	binDir := t.TempDir()
	mmcli := filepath.Join(binDir, "mmcli")
	if err := os.WriteFile(mmcli, []byte("#!/bin/sh\nprintf '%s\\n' '{\"modem-list\":[]}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := discoverModemID(context.Background()); err == nil {
		t.Fatal("discoverModemID() unexpectedly succeeded")
	}
}
