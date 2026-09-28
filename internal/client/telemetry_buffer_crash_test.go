package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"radio-gaga/internal/models"
)

func TestTransmitBufferPeriodicallyRejectsZeroPeriod(t *testing.T) {
	s, _ := newTestClient(t)
	s.config.Telemetry.Buffer.Enabled = true
	s.config.Telemetry.TransmitPeriod = "0s"
	ctx, cancel := context.WithCancel(s.ctx)
	cancel()
	s.ctx = ctx
	s.transmitBufferPeriodically()
}

func TestTelemetryBufferRejectsMissingEventData(t *testing.T) {
	s, _ := newTestClient(t)
	s.config.Telemetry.Buffer.Enabled = true
	badBuffer := `{"events":[{"data":null}]}`

	if err := s.redisClient.Set(context.Background(), telemetryBufferKey, badBuffer, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadBufferFromRedis(); err == nil || !strings.Contains(err.Error(), "event 0 has no data") {
		t.Fatalf("Redis load error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "buffer.json")
	s.config.Telemetry.Buffer.PersistPath = path
	if err := os.WriteFile(path, []byte(badBuffer), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadBufferFromDisk(); err == nil || !strings.Contains(err.Error(), "event 0 has no data") {
		t.Fatalf("disk load error = %v", err)
	}

	s.buffer = &models.TelemetryBuffer{Events: []models.BufferedTelemetryEvent{{}}}
	if err := s.transmitBuffer(); err == nil || !strings.Contains(err.Error(), "event 0 has no data") {
		t.Fatalf("transmit error = %v", err)
	}
	if err := s.addTelemetryToBuffer(nil); err == nil {
		t.Fatal("accepted nil telemetry data")
	}
}
