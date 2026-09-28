package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
)

func TestGetTelemetryIntervalRejectsZero(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()
	if err := client.HSet(ctx, "vehicle", "state", "ready-to-drive").Err(); err != nil {
		t.Fatal(err)
	}
	cfg := &models.Config{}
	cfg.Telemetry.Intervals.Driving = "0s"
	if interval, reason := GetTelemetryInterval(ctx, client, cfg); interval != time.Minute || reason != "fallback" {
		t.Fatalf("interval = %v (%s), want 1m fallback", interval, reason)
	}
}

func TestSysInfoFromProc(t *testing.T) {
	if uptime := readUptimeSeconds(); uptime <= 0 {
		t.Errorf("readUptimeSeconds() = %v, want > 0", uptime)
	}

	bootID := readBootID()
	if bootID == "" {
		t.Fatal("readBootID() returned empty")
	}
	if len(bootID) != 36 || strings.Count(bootID, "-") != 4 {
		t.Errorf("readBootID() = %q, want a UUID", bootID)
	}
}

func TestUptimeSecondsIsCached(t *testing.T) {
	first := uptimeSeconds(time.Hour)
	if first <= 0 {
		t.Fatalf("uptimeSeconds() = %v, want > 0", first)
	}
	if second := uptimeSeconds(time.Hour); second != first {
		t.Errorf("uptimeSeconds() = %v on second call, want cached %v", second, first)
	}
}

func TestECUVersionFromHash(t *testing.T) {
	t.Run("omits unreported firmware", func(t *testing.T) {
		if got := ecuVersionFromHash(map[string]string{}); got != nil {
			t.Fatalf("ECU version = %#v, want nil", got)
		}
	})

	t.Run("decodes the identification block", func(t *testing.T) {
		got := ecuVersionFromHash(map[string]string{
			"fw-version":           "0445400C",
			"fw:base-version":      "4.0",
			"fw:app-version":       "12",
			"motor:rated-power-kw": "4",
			"motor:max-speed-kmh":  "45",
			"warranty-date":        "20240101",
		})
		if got == nil {
			t.Fatal("ECU version = nil, want populated version")
		}
		if got.FirmwareVersion != "0445400C" || got.BaseVersion != "4.0" || got.AppVersion != "12" {
			t.Errorf("ECU version = %#v", got)
		}
		if got.MotorRatedPowerKW != 4 || got.MotorMaxSpeedKMH != 45 || got.WarrantyDate != "20240101" {
			t.Errorf("ECU identification = %#v", got)
		}
	})
}

func TestGetECUVersion(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()

	got, err := GetECUVersion(ctx, client)
	if err != nil {
		t.Fatalf("GetECUVersion() error = %v", err)
	}
	if got != nil {
		t.Fatalf("GetECUVersion() = %#v, want nil before firmware is reported", got)
	}

	server.HSet("engine-ecu", "fw-version", "0445400C", "fw:base-version", "4.0")
	got, err = GetECUVersion(ctx, client)
	if err != nil {
		t.Fatalf("GetECUVersion() error = %v", err)
	}
	if got == nil || got.FirmwareVersion != "0445400C" || got.BaseVersion != "4.0" {
		t.Errorf("GetECUVersion() = %#v", got)
	}
}

func TestNavigationRouteSupported(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()

	if navigationRouteSupported(client, ctx) {
		t.Fatal("missing registry advertised route support")
	}
	server.HSet("system", "capabilities", "cap:ext:nav=2:keycard")
	if !navigationRouteSupported(client, ctx) {
		t.Fatal("nav=2 was not reported")
	}
	server.HSet("system", "capabilities", "cap:ext:nav=20:keycard")
	if navigationRouteSupported(client, ctx) {
		t.Fatal("nav=20 was reported as nav=2")
	}
}

func TestAuxBatteryFromHash(t *testing.T) {
	t.Run("omits missing startup data", func(t *testing.T) {
		if got := auxBatteryFromHash(map[string]string{}); got != nil {
			t.Fatalf("aux battery = %#v, want nil", got)
		}
	})

	t.Run("preserves a valid zero state of charge", func(t *testing.T) {
		got := auxBatteryFromHash(map[string]string{
			"charge":        "0",
			"voltage":       "10900",
			"charge-status": "not-charging",
		})
		if got == nil {
			t.Fatal("aux battery = nil, want populated battery")
		}
		if got.Level != 0 || got.Voltage != 10900 || got.ChargeStatus != "not-charging" {
			t.Errorf("aux battery = %#v", got)
		}
	})
}

func TestCBBatteryFromHash(t *testing.T) {
	t.Run("omits unpopulated startup data", func(t *testing.T) {
		if got := cbbBatteryFromHash(map[string]string{}); got != nil {
			t.Fatalf("CBB battery = %#v, want nil", got)
		}
	})

	t.Run("reports explicit absence", func(t *testing.T) {
		got := cbbBatteryFromHash(map[string]string{"present": "false"})
		if got == nil || got.Present == nil || *got.Present {
			t.Fatalf("CBB battery = %#v, want present=false", got)
		}
	})

	t.Run("accepts a measured CBB without presence", func(t *testing.T) {
		got := cbbBatteryFromHash(map[string]string{"charge": "100", "cell-voltage": "4200000"})
		if got == nil || got.Present != nil || got.Level != 100 || got.CellVoltage != 4200000 {
			t.Errorf("CBB battery = %#v", got)
		}
	})
}
