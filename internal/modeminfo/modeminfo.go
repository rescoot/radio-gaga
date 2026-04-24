// Package modeminfo fetches static modem hardware identity via mmcli.
//
// ModemManager on our scooters runs with --debug, so `mmcli --command` is
// available for vendor AT commands. The SIM7100E exposes its SIMCOM firmware
// revision only via AT+SIMCOMATI — the `firmware-revision` field in mmcli's
// generic info is the underlying Qualcomm baseband (AT+CGMR), not the SIMCOM
// build.
package modeminfo

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

type Info struct {
	Manufacturer           string `json:"manufacturer,omitempty"`
	Model                  string `json:"model,omitempty"`
	HardwareRevision       string `json:"hardware_revision,omitempty"`
	FirmwareRevision       string `json:"firmware_revision,omitempty"`
	VendorFirmwareRevision string `json:"vendor_firmware_revision,omitempty"`
	DeviceID               string `json:"device_id,omitempty"`
	EquipmentID            string `json:"equipment_id,omitempty"`
	OwnNumber              string `json:"own_number,omitempty"`
	SupportedModes         string `json:"supported_modes,omitempty"`
	CurrentModes           string `json:"current_modes,omitempty"`
}

var cached atomic.Pointer[Info]

// Get returns the cached modem info, or nil if the poller hasn't succeeded yet.
func Get() *Info {
	return cached.Load()
}

// StartPoller spawns a goroutine that retries Fetch until it succeeds or ctx is
// cancelled. Intended to be called once at process start; the modem USB device
// can take a while to enumerate after boot.
func StartPoller(ctx context.Context) {
	go func() {
		const interval = 30 * time.Second
		for {
			info, err := Fetch(ctx)
			if err == nil {
				cached.Store(info)
				log.Printf("modeminfo: %s %s hw=%s fw=%s vendor_fw=%s imei=%s",
					info.Manufacturer, info.Model, info.HardwareRevision,
					info.FirmwareRevision, info.VendorFirmwareRevision, info.EquipmentID)
				return
			}
			if ctx.Err() != nil {
				return
			}
			log.Printf("modeminfo: not available yet: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// mmcliModemEnvelope mirrors the subset of `mmcli -J -m any` we care about.
type mmcliModemEnvelope struct {
	Modem struct {
		Generic struct {
			Manufacturer         string   `json:"manufacturer"`
			Model                string   `json:"model"`
			HardwareRevision     string   `json:"hardware-revision"`
			FirmwareRevision     string   `json:"firmware-revision"`
			DeviceIdentifier     string   `json:"device-identifier"`
			EquipmentIdentifier  string   `json:"equipment-identifier"`
			OwnNumbers           []string `json:"own-numbers"`
			SupportedModes       []string `json:"supported-modes"`
			CurrentModes         string   `json:"current-modes"`
		} `json:"generic"`
	} `json:"modem"`
}

// Fetch performs one attempt to read modem identity via mmcli. Returns an error
// if no modem is present or mmcli is unavailable.
func Fetch(ctx context.Context) (*Info, error) {
	var env mmcliModemEnvelope
	if err := runMMCLIJSON(ctx, &env, "-J", "-m", "any"); err != nil {
		return nil, err
	}
	g := env.Modem.Generic
	info := &Info{
		Manufacturer:     g.Manufacturer,
		Model:            g.Model,
		HardwareRevision: g.HardwareRevision,
		FirmwareRevision: g.FirmwareRevision,
		DeviceID:         g.DeviceIdentifier,
		EquipmentID:      g.EquipmentIdentifier,
		OwnNumber:        firstNonEmpty(g.OwnNumbers),
		SupportedModes:   strings.Join(g.SupportedModes, "; "),
		CurrentModes:     g.CurrentModes,
	}

	if rev, err := fetchVendorFirmware(ctx); err != nil {
		log.Printf("modeminfo: AT+SIMCOMATI failed: %v", err)
	} else {
		info.VendorFirmwareRevision = rev
	}

	return info, nil
}

var revisionRE = regexp.MustCompile(`(?m)^\s*Revision:\s*(.+?)\s*$`)

// fetchVendorFirmware runs AT+SIMCOMATI via mmcli and extracts the Revision
// line. mmcli's `--command` always returns YAML-wrapped plain text (even with
// `-J`), so we just regex-scrape the raw output.
func fetchVendorFirmware(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "mmcli", "-m", "any", "--command=AT+SIMCOMATI")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mmcli --command=AT+SIMCOMATI: %v: %s", err, strings.TrimSpace(string(raw)))
	}
	if m := revisionRE.FindStringSubmatch(string(raw)); len(m) == 2 {
		return strings.TrimSpace(m[1]), nil
	}
	return "", fmt.Errorf("Revision line not found in AT+SIMCOMATI response")
}

func runMMCLIJSON(ctx context.Context, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, "mmcli", args...)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mmcli %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mmcli %s: parse json: %v", strings.Join(args, " "), err)
	}
	return nil
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if s != "" && s != "--" {
			return s
		}
	}
	return ""
}
