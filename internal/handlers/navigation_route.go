package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
)

type routeStop struct {
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
	Label string  `json:"label,omitempty"`
}

// A distinct command name lets older MQTT clients reject routes without
// interpreting missing latitude/longitude as a request to clear navigation.
func handleNavigateRouteCommand(client *redis.Client, ctx context.Context, params map[string]interface{}) error {
	stops, err := parseRouteStops(params["waypoints"])
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(stops)
	if err != nil {
		return err
	}
	first := stops[0]
	fields := map[string]interface{}{
		"waypoints":    string(encoded),
		"current-step": "0",
		"latitude":     strconv.FormatFloat(first.Lat, 'f', -1, 64),
		"longitude":    strconv.FormatFloat(first.Lon, 'f', -1, 64),
		"destination":  fmt.Sprintf("%.6f,%.6f", first.Lat, first.Lon),
		"address":      first.Label,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	}
	if err := client.HSet(ctx, "navigation", fields).Err(); err != nil {
		return err
	}
	return client.Publish(ctx, "navigation", "updated").Err()
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
