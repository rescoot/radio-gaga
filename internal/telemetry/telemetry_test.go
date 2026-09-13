package telemetry

import "testing"

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
