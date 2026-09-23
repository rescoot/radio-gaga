package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
)

func TestNavigateRouteCommand(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()

	params := map[string]interface{}{"waypoints": []interface{}{
		map[string]interface{}{"latitude": 52.51, "longitude": 13.41, "label": "Work"},
		map[string]interface{}{"lat": 52.52, "lon": 13.42, "name": "Home"},
	}}
	if err := handleNavigateRouteCommand(client, ctx, params); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("navigation", "waypoints"); got != `[{"lat":52.51,"lon":13.41,"label":"Work"},{"lat":52.52,"lon":13.42,"label":"Home"}]` {
		t.Fatalf("waypoints = %s", got)
	}
	for field, want := range map[string]string{"current-step": "0", "latitude": "52.51", "longitude": "13.41", "address": "Work", "destination": "52.510000,13.410000"} {
		if got := server.HGet("navigation", field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}

	if err := handleNavigateCommand(client, ctx, map[string]interface{}{"latitude": 53.0, "longitude": 14.0}, ""); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("navigation", "waypoints"); got != "" {
		t.Errorf("single destination left stale waypoints: %s", got)
	}
	if err := handleNavigateRouteCommand(client, ctx, params); err != nil {
		t.Fatal(err)
	}
	if err := handleNavigateCommand(client, ctx, map[string]interface{}{}, ""); err != nil {
		t.Fatal(err)
	}
	if got := server.HGet("navigation", "waypoints"); got != "" {
		t.Errorf("clear left stale waypoints: %s", got)
	}
}

func TestNavigateRouteRejectsInvalidStopsWithoutClearingDestination(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	ctx := context.Background()
	server.HSet("navigation", "destination", "52.000000,13.000000")

	for _, raw := range []interface{}{
		nil, []interface{}{}, []interface{}{map[string]interface{}{"latitude": 91.0, "longitude": 13.0}},
		[]interface{}{map[string]interface{}{"latitude": 52.0}},
		[]interface{}{map[string]interface{}{"lat": "NaN", "lon": 13.0}},
		[]interface{}{map[string]interface{}{"lat": 52.0, "lon": 13.0, "label": strings.Repeat("x", 201)}},
		make([]interface{}, 26),
	} {
		if err := handleNavigateRouteCommand(client, ctx, map[string]interface{}{"waypoints": raw}); err == nil {
			t.Errorf("accepted invalid waypoints: %v", raw)
		}
		if got := server.HGet("navigation", "destination"); got != "52.000000,13.000000" {
			t.Fatalf("invalid route changed destination: %s", got)
		}
	}
}
