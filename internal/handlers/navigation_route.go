package handlers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	redis_ipc "github.com/librescoot/redis-ipc"
)

type routeStop struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Label string  `json:"label,omitempty"`
}

// A distinct command name lets older MQTT clients reject routes without
// interpreting missing latitude/longitude as a request to clear navigation.
func handleNavigateRouteCommand(client *redis.Client, ctx context.Context, params map[string]interface{}) error {
	capabilities, err := client.HGet(ctx, "system", "capabilities").Result()
	if err != nil || !routeCapabilityAdvertised(capabilities) {
		return fmt.Errorf("scooter does not advertise multi-stop routes")
	}
	stops, err := parseRouteStops(params["waypoints"])
	if err != nil {
		return err
	}
	return callRoutePlan(client, ctx, "plan.replace", struct {
		Stops []routeStop `json:"stops"`
	}{Stops: stops})
}

// callRoutePlan uses the settings-service RPC; the navigation hash is its projection.
func callRoutePlan(client *redis.Client, ctx context.Context, method string, request interface{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ipc, err := redis_ipc.New(redis_ipc.WithURL(client.Options().Addr), redis_ipc.WithDialTimeout(time.Second))
	if err != nil {
		return fmt.Errorf("route-plan service unavailable: %w", err)
	}
	defer ipc.Close()
	_, err = redis_ipc.CallMethod[interface{}, struct{}](ipc, "settings:route-plan", method, request, 2*time.Second)
	if err != nil {
		return fmt.Errorf("route-plan %s: %w", method, err)
	}
	return nil
}

func routeCapabilityAdvertised(capabilities string) bool {
	if !strings.HasPrefix(capabilities, "cap:ext:") {
		return false
	}
	for _, group := range strings.Split(strings.TrimPrefix(capabilities, "cap:ext:"), ":") {
		if group == "nav=2" {
			return true
		}
	}
	return false
}

func parseRouteStops(raw interface{}) ([]routeStop, error) {
	list, ok := raw.([]interface{})
	if !ok || len(list) == 0 || len(list) > 25 {
		return nil, fmt.Errorf("waypoints must contain 1 to 25 stops")
	}
	stops := make([]routeStop, 0, len(list))
	for _, entry := range list {
		stop, ok := entry.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid waypoint")
		}
		lat, okLat := routeCoordinate(stop, "latitude", "lat", 90)
		lon, okLon := routeCoordinate(stop, "longitude", "lon", 180)
		if !okLat || !okLon {
			return nil, fmt.Errorf("invalid waypoint coordinates")
		}
		label := stop["label"]
		if label == nil {
			label = stop["name"]
		}
		name, ok := label.(string)
		if label != nil && (!ok || len(name) > 200) {
			return nil, fmt.Errorf("invalid waypoint label")
		}
		stops = append(stops, routeStop{Lat: lat, Lon: lon, Label: name})
	}
	return stops, nil
}

func routeCoordinate(stop map[string]interface{}, primary, fallback string, limit float64) (float64, bool) {
	value, exists := stop[primary]
	if !exists {
		value, exists = stop[fallback]
	}
	if !exists {
		return 0, false
	}
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case string:
		var err error
		number, err = strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0) && math.Abs(number) <= limit
}
