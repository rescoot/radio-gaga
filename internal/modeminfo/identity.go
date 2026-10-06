package modeminfo

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// Identity describes the active modem and the SIM selected by that modem.
type Identity struct {
	IMEI  string `json:"imei"`
	ICCID string `json:"iccid"`
	IMSI  string `json:"imsi,omitempty"`
}

var (
	imeiPattern    = regexp.MustCompile(`^[0-9]{15}$`)
	iccidPattern   = regexp.MustCompile(`^[0-9]{19,20}$`)
	imsiPattern    = regexp.MustCompile(`^[0-9]{15}$`)
	simPathPattern = regexp.MustCompile(`^/org/freedesktop/ModemManager1/SIM/[0-9]+$`)
)

func FetchIdentity(ctx context.Context) (Identity, error) {
	var list mmcliModemList
	if err := runMMCLIJSON(ctx, &list, "-J", "-L"); err != nil {
		return Identity{}, err
	}
	if len(list.Modems) != 1 {
		return Identity{}, fmt.Errorf("identity requires exactly one modem")
	}

	var modem struct {
		Modem struct {
			Generic struct {
				EquipmentID string `json:"equipment-identifier"`
				SIM         string `json:"sim"`
			} `json:"generic"`
		} `json:"modem"`
	}
	if err := runMMCLIJSON(ctx, &modem, "-J", "-m", list.Modems[0]); err != nil {
		return Identity{}, err
	}
	if !simPathPattern.MatchString(modem.Modem.Generic.SIM) {
		return Identity{}, fmt.Errorf("modem has no active SIM object")
	}
	var sim struct {
		SIM struct {
			Properties struct {
				ICCID string `json:"iccid"`
				IMSI  string `json:"imsi"`
			} `json:"properties"`
		} `json:"sim"`
	}
	if err := runMMCLIJSON(ctx, &sim, "-J", "-i", modem.Modem.Generic.SIM); err != nil {
		return Identity{}, err
	}
	identity := Identity{
		IMEI:  strings.TrimSpace(modem.Modem.Generic.EquipmentID),
		ICCID: strings.TrimSpace(sim.SIM.Properties.ICCID),
		IMSI:  strings.TrimSpace(sim.SIM.Properties.IMSI),
	}
	if identity.IMSI == "--" {
		identity.IMSI = ""
	}
	if !imeiPattern.MatchString(identity.IMEI) || !iccidPattern.MatchString(identity.ICCID) ||
		(identity.IMSI != "" && !imsiPattern.MatchString(identity.IMSI)) {
		return Identity{}, fmt.Errorf("modem returned invalid identity fields")
	}
	return identity, nil
}
