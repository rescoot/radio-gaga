package sms

import (
	"testing"

	"radio-gaga/internal/models"
)

func TestNewNotifierRejectsNonPositiveRateLimit(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		cfg := &models.SMSConfig{RateLimit: value}
		if _, err := NewNotifier(cfg, &models.ScooterConfig{}); err == nil {
			t.Fatalf("accepted rate_limit %q", value)
		}
	}
}
