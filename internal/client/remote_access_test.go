package client

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestWriteCloudStatusConvergesAcrossProviders(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()

	if err := client.HSet(ctx, "remote-access", "uplink-service", "connected").Err(); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudStatus(ctx, client, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("remote-access", "status"); got != "connected" {
		t.Fatalf("status with another connected provider = %q, want connected", got)
	}
	if got := server.HGet("remote-access", "radio-gaga"); got != "disconnected" {
		t.Fatalf("radio-gaga = %q, want disconnected", got)
	}
	if got := server.HGet("internet", "unu-cloud"); got != "disconnected" {
		t.Fatalf("legacy unu-cloud = %q, want disconnected", got)
	}

	if err := client.HSet(ctx, "remote-access", "uplink-service", "disconnected").Err(); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudStatus(ctx, client, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("remote-access", "status"); got != "disconnected" {
		t.Fatalf("status with all providers disconnected = %q, want disconnected", got)
	}
}
