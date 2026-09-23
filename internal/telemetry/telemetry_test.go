package telemetry

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

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
