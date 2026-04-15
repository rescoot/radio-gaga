package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

const suppressionWindow = 2 * time.Second

// Location represents a saved location on the scooter dashboard.
type Location struct {
	Slot       int     `json:"slot"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Label      string  `json:"label"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt string  `json:"last_used_at"`
}

// PushRequest is the JSON body sent to the Sunshine locations endpoint.
type PushRequest struct {
	Locations []Location `json:"locations"`
}

// LocationPusher reads saved locations from Redis and pushes them to
// the Sunshine HTTP API whenever they change locally.
type LocationPusher struct {
	redisClient   *redis.Client
	apiBaseURL    string
	scooterID     string
	token         string
	httpClient    *http.Client
	mu            sync.Mutex
	suppressUntil time.Time
}

// NewLocationPusher creates a LocationPusher configured with the given
// Redis client, API base URL, scooter database ID, auth token, and
// HTTP timeout.
func NewLocationPusher(redisClient *redis.Client, apiBaseURL, scooterID, token string, timeout time.Duration) *LocationPusher {
	return &LocationPusher{
		redisClient: redisClient,
		apiBaseURL:  strings.TrimRight(apiBaseURL, "/"),
		scooterID:   scooterID,
		token:       token,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// ReadLocalLocations reads all dashboard.saved-locations.* fields from
// the Redis "settings" hash and parses them into Location structs.
// Fields are stored as individual hash entries:
//
//	dashboard.saved-locations.0.latitude  → "52.476382"
//	dashboard.saved-locations.0.longitude → "13.367518"
//	dashboard.saved-locations.0.label     → "Home"
//	etc.
func (p *LocationPusher) ReadLocalLocations(ctx context.Context) ([]Location, error) {
	all, err := p.redisClient.HGetAll(ctx, "settings").Result()
	if err != nil {
		return nil, fmt.Errorf("reading settings hash: %w", err)
	}

	const prefix = "dashboard.saved-locations."
	slots := make(map[int]*Location)

	for key, val := range all {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		// rest is e.g. "0.latitude" or "2.label"
		parts := strings.SplitN(rest, ".", 2)
		if len(parts) != 2 {
			continue
		}
		slot, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		field := parts[1]

		if slots[slot] == nil {
			slots[slot] = &Location{Slot: slot}
		}
		switch field {
		case "latitude":
			slots[slot].Latitude, _ = strconv.ParseFloat(val, 64)
		case "longitude":
			slots[slot].Longitude, _ = strconv.ParseFloat(val, 64)
		case "label":
			slots[slot].Label = val
		case "created-at":
			slots[slot].CreatedAt = val
		case "last-used-at":
			slots[slot].LastUsedAt = val
		}
	}

	var locations []Location
	for _, loc := range slots {
		if loc.Latitude != 0 || loc.Longitude != 0 {
			locations = append(locations, *loc)
		}
	}
	return locations, nil
}

// Push reads local locations and PUTs them to the Sunshine API.
func (p *LocationPusher) Push(ctx context.Context) error {
	locations, err := p.ReadLocalLocations(ctx)
	if err != nil {
		return fmt.Errorf("reading local locations: %w", err)
	}

	body, err := json.Marshal(PushRequest{Locations: locations})
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/scooters/%s/locations", p.apiBaseURL, p.scooterID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	p.markPushed()
	log.Printf("Pushed %d locations to %s", len(locations), url)
	return nil
}

// ShouldPush returns false during the suppression window after a
// recent push, preventing feedback loops when the server writes
// locations back via command.
func (p *LocationPusher) ShouldPush() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().After(p.suppressUntil)
}

// Suppress sets the suppression window from now, used by command
// handlers that write locations to Redis to prevent echo pushes.
func (p *LocationPusher) Suppress() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suppressUntil = time.Now().Add(suppressionWindow)
}

func (p *LocationPusher) markPushed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suppressUntil = time.Now().Add(suppressionWindow)
}
