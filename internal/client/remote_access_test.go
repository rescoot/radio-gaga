package client

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestWriteCloudStatusWritesProviderState(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()

	if err := client.HSet(ctx, "remote-access", "uplink-service", "connected", "status", "connected").Err(); err != nil {
		t.Fatal(err)
	}
	if err := writeCloudStatus(ctx, client, "disconnected"); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("remote-access", "radio-gaga"); got != "disconnected" {
		t.Fatalf("radio-gaga = %q, want disconnected", got)
	}
	if got := server.HGet("remote-access", "uplink-service"); got != "connected" {
		t.Fatalf("uplink-service = %q, want connected", got)
	}
	if exists, err := client.HExists(ctx, "remote-access", "status").Result(); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("legacy aggregate status was not removed")
	}
	if got := server.HGet("internet", "unu-cloud"); got != "disconnected" {
		t.Fatalf("legacy unu-cloud = %q, want disconnected", got)
	}

	if err := writeCloudStatus(ctx, client, "connected"); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("remote-access", "radio-gaga"); got != "connected" {
		t.Fatalf("radio-gaga = %q, want connected", got)
	}
	if got := server.HGet("internet", "unu-cloud"); got != "connected" {
		t.Fatalf("legacy unu-cloud = %q, want connected", got)
	}
}
