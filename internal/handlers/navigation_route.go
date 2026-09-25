package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// callRoutePlan uses the existing Redis connection so configured authentication,
// database, and TLS settings also apply to route-plan requests.
func callRoutePlan(client *redis.Client, ctx context.Context, method string, request interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("route-plan request ID: %w", err)
	}
	id := hex.EncodeToString(nonce[:])
	channel := "settings:route-plan"
	replyChannel := channel + ":reply:" + id
	deadline, _ := ctx.Deadline()
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("route-plan request: %w", err)
	}
	envelope, err := json.Marshal(struct {
		ID           string          `json:"id"`
		Method       string          `json:"method"`
		ReplyChannel string          `json:"reply_channel"`
		Deadline     int64           `json:"deadline"`
		Payload      json.RawMessage `json:"payload"`
	}{id, method, replyChannel, deadline.UnixMilli(), payload})
	if err != nil {
		return fmt.Errorf("route-plan envelope: %w", err)
	}
	sub := client.Subscribe(ctx, replyChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return fmt.Errorf("route-plan subscribe: %w", err)
	}
	if err := client.LPush(ctx, channel, envelope).Err(); err != nil {
		return fmt.Errorf("route-plan request: %w", err)
	}
	msg, err := sub.ReceiveMessage(ctx)
	if err != nil {
		return fmt.Errorf("route-plan response: %w", err)
	}
	var reply struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(msg.Payload), &reply); err != nil {
		return fmt.Errorf("route-plan response: %w", err)
	}
	if !reply.OK {
		return fmt.Errorf("route-plan %s: %s", method, reply.Error)
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
