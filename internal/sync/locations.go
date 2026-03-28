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
func (p *LocationPusher) ReadLocalLocations(ctx context.Context) ([]Location, error) {
	all, err := p.redisClient.HGetAll(ctx, "settings").Result()
	if err != nil {
		return nil, fmt.Errorf("reading settings hash: %w", err)
	}

	var locations []Location
	for key, val := range all {
		if !strings.HasPrefix(key, "dashboard.saved-locations.") {
			continue
		}

		loc, err := parseLocationField(key, val)
		if err != nil {
			log.Printf("Skipping malformed location field %s: %v", key, err)
			continue
		}
		locations = append(locations, loc)
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

// parseLocationField parses a Redis settings field like
// "dashboard.saved-locations.1" with value "lat,lng,label,created_at,last_used_at"
// into a Location struct.
func parseLocationField(key, val string) (Location, error) {
	parts := strings.SplitN(key, ".", 3)
	if len(parts) < 3 {
		return Location{}, fmt.Errorf("unexpected key format: %s", key)
	}
	slotStr := parts[2]
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		return Location{}, fmt.Errorf("non-integer slot %q: %w", slotStr, err)
	}

	fields := strings.SplitN(val, ",", 5)
	if len(fields) < 2 {
		return Location{}, fmt.Errorf("need at least lat,lng in value")
	}

	lat, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Location{}, fmt.Errorf("parsing latitude: %w", err)
	}
	lng, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return Location{}, fmt.Errorf("parsing longitude: %w", err)
	}

	loc := Location{
		Slot:      slot,
		Latitude:  lat,
		Longitude: lng,
	}
	if len(fields) > 2 {
		loc.Label = fields[2]
	}
	if len(fields) > 3 {
		loc.CreatedAt = fields[3]
	}
	if len(fields) > 4 {
		loc.LastUsedAt = fields[4]
	}

	return loc, nil
}
