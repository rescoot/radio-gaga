package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"radio-gaga/internal/models"
)

func TestSuccessfulBufferResetClearsStaleDiskFallbackWhenRedisWorks(t *testing.T) {
	s, _ := newTestClient(t)
	path := filepath.Join(t.TempDir(), "buffer.json")
	s.config.Telemetry.Buffer.PersistPath = path
	if err := os.WriteFile(path, []byte(`{"events":[{"data":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	reset := &models.TelemetryBuffer{Events: []models.BufferedTelemetryEvent{}, BatchID: "new-batch"}
	s.persistTransmittedBufferReset(reset)

	stored, err := s.loadBufferFromDisk()
	if err != nil || len(stored.Events) != 0 || stored.BatchID != reset.BatchID {
		t.Fatalf("stale disk fallback was not replaced: events=%d err=%v", len(stored.Events), err)
	}
	if err := s.redisClient.Del(context.Background(), telemetryBufferKey).Err(); err != nil {
		t.Fatal(err)
	}
	fallback, err := s.loadBufferFromDisk()
	if err != nil || len(fallback.Events) != 0 {
		t.Fatalf("Redis loss restored an old disk buffer: err=%v", err)
	}
}
