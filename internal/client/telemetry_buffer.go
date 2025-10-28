package client

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"radio-gaga/internal/models"
	"radio-gaga/internal/telemetry"
)

const (
	telemetryBufferKey = "radio-gaga:telemetry-buffer"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// initTelemetryBuffer initializes the telemetry buffer
func (s *ScooterMQTTClient) initTelemetryBuffer() {
	// If buffer is not enabled, return
	if !s.config.Telemetry.Buffer.Enabled {
		log.Printf("Telemetry buffer is disabled")
		return
	}

	// Try to load buffer from Redis
	buffer, err := s.loadBufferFromRedis()
	if err != nil {
		log.Printf("Failed to load buffer from Redis: %v", err)
		// Try to load from disk if persist path is set
		if s.config.Telemetry.Buffer.PersistPath != "" {
			buffer, err = s.loadBufferFromDisk()
			if err != nil {
				log.Printf("Failed to load buffer from disk: %v", err)
				// Create a new buffer
				buffer = s.createNewBuffer()
			} else {
				log.Printf("Loaded buffer from disk with %d events", len(buffer.Events))
			}
		} else {
			// Create a new buffer
			buffer = s.createNewBuffer()
		}
	} else {
		log.Printf("Loaded buffer from Redis with %d events", len(buffer.Events))
	}

	// Set the in-memory buffer
	s.buffer = buffer

	// Persist initial buffer state (best effort)
	if err := s.saveBufferToRedis(buffer); err != nil {
		log.Printf("Warning: failed to persist initial buffer to Redis: %v", err)
	}

	// Start buffer transmission goroutine
	go s.transmitBufferPeriodically()
}

// createNewBuffer creates a new telemetry buffer
func (s *ScooterMQTTClient) createNewBuffer() *models.TelemetryBuffer {
	// Generate a random batch ID
	batchID, err := generateRandomID()
	if err != nil {
		log.Printf("Failed to generate batch ID: %v", err)
		batchID = fmt.Sprintf("batch-%d", time.Now().UnixNano())
	}

	now := time.Now()
	return &models.TelemetryBuffer{
		Events:          []models.BufferedTelemetryEvent{},
		BatchID:         batchID,
		CreatedAt:       now,
		SequenceCounter: 0,
		LastSystemTime:  now,
	}
}

// loadBufferFromRedis loads the telemetry buffer from Redis
func (s *ScooterMQTTClient) loadBufferFromRedis() (*models.TelemetryBuffer, error) {
	// Get buffer from Redis
	bufferJSON, err := s.redisClient.Get(s.ctx, telemetryBufferKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get buffer from Redis: %v", err)
	}

	// Unmarshal buffer
	buffer := &models.TelemetryBuffer{}
	if err := json.Unmarshal([]byte(bufferJSON), buffer); err != nil {
		return nil, fmt.Errorf("failed to unmarshal buffer: %v", err)
	}

	return buffer, nil
}

// saveBufferToRedis saves the telemetry buffer to Redis
func (s *ScooterMQTTClient) saveBufferToRedis(buffer *models.TelemetryBuffer) error {
	// Marshal buffer
	bufferJSON, err := json.Marshal(buffer)
	if err != nil {
		return fmt.Errorf("failed to marshal buffer: %v", err)
	}

	// Save buffer to Redis
	if err := s.redisClient.Set(s.ctx, telemetryBufferKey, bufferJSON, 0).Err(); err != nil {
		return fmt.Errorf("failed to save buffer to Redis: %v", err)
	}

	return nil
}

// loadBufferFromDisk loads the telemetry buffer from disk
func (s *ScooterMQTTClient) loadBufferFromDisk() (*models.TelemetryBuffer, error) {
	// Check if persist path is set
	if s.config.Telemetry.Buffer.PersistPath == "" {
		return nil, fmt.Errorf("persist path not set")
	}

	// Read buffer from disk
	bufferJSON, err := os.ReadFile(s.config.Telemetry.Buffer.PersistPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read buffer from disk: %v", err)
	}

	// Unmarshal buffer
	buffer := &models.TelemetryBuffer{}
	if err := json.Unmarshal(bufferJSON, buffer); err != nil {
		return nil, fmt.Errorf("failed to unmarshal buffer: %v", err)
	}

	return buffer, nil
}

// saveBufferToDisk saves the telemetry buffer to disk
func (s *ScooterMQTTClient) saveBufferToDisk(buffer *models.TelemetryBuffer) error {
	// Check if persist path is set
	if s.config.Telemetry.Buffer.PersistPath == "" {
		return fmt.Errorf("persist path not set")
	}

	// Marshal buffer
	bufferJSON, err := json.Marshal(buffer)
	if err != nil {
		return fmt.Errorf("failed to marshal buffer: %v", err)
	}

	// Ensure the persist path is not a directory
	persistPath := s.config.Telemetry.Buffer.PersistPath
	if info, err := os.Stat(persistPath); err == nil && info.IsDir() {
		if err := os.RemoveAll(persistPath); err != nil {
			return fmt.Errorf("failed to remove existing directory: %v", err)
		}
	}

	// Create parent directory
	dir := filepath.Dir(persistPath)
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	// Write buffer to disk
	if err := os.WriteFile(persistPath, bufferJSON, 0644); err != nil {
		return fmt.Errorf("failed to write buffer to disk: %v", err)
	}

	return nil
}

// addTelemetryToBuffer adds a telemetry event to the buffer
func (s *ScooterMQTTClient) addTelemetryToBuffer(data *models.TelemetryData) error {
	// If buffer is not enabled, return
	if !s.config.Telemetry.Buffer.Enabled {
		return nil
	}

	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()

	// Use in-memory buffer if available, otherwise load from persistence
	var buffer *models.TelemetryBuffer
	if s.buffer != nil {
		buffer = s.buffer
	} else {
		var err error
		buffer, err = s.loadBufferFromRedis()
		if err != nil {
			log.Printf("Failed to load buffer from Redis: %v", err)
			if s.config.Telemetry.Buffer.PersistPath != "" {
				buffer, err = s.loadBufferFromDisk()
				if err != nil {
					log.Printf("Failed to load buffer from disk: %v", err)
					buffer = s.createNewBuffer()
				} else {
					log.Printf("Loaded buffer from disk with %d events", len(buffer.Events))
				}
			} else {
				buffer = s.createNewBuffer()
			}
		}
		s.buffer = buffer
	}

	now := time.Now()
	
	// Check for significant backward time jump or unreasonably large forward jump
	// Backward jump: any negative time difference
	// Large forward jump: more than 10 minutes since last update (suggesting NTP correction)
	timeSinceLastUpdate := now.Sub(buffer.LastSystemTime)
	isBackwardJump := timeSinceLastUpdate < 0
	isLargeForwardJump := timeSinceLastUpdate > 10*time.Minute
	
	if (isBackwardJump || isLargeForwardJump) && len(buffer.Events) > 0 {
		// Calculate the clock correction (how much the system clock jumped)
		clockCorrection := now.Sub(buffer.LastSystemTime)
		log.Printf("Detected time jump of %.1f seconds, recalibrating buffer timestamps", clockCorrection.Seconds())

		// Update serviceStartTime to account for the clock correction
		// This is critical: the relative offsets were calculated using the wrong clock,
		// but they represent real elapsed time. By correcting serviceStartTime, we make
		// serviceStartTime + offset give us the correct absolute time.
		oldServiceStart := s.serviceStartTime
		s.serviceStartTime = s.serviceStartTime.Add(clockCorrection)
		log.Printf("Updated serviceStartTime from %s to %s (correction: %.1f seconds)",
			oldServiceStart.Format(time.RFC3339),
			s.serviceStartTime.Format(time.RFC3339),
			clockCorrection.Seconds())

		// Recalibrate all existing events in this buffer
		for i := range buffer.Events {
			event := &buffer.Events[i]

			// Only recalibrate relative timestamps from this session
			if strings.HasPrefix(event.Data.Timestamp, "INVALID_RELATIVE:") {
				// Parse the relative offset
				offsetStr := strings.TrimPrefix(event.Data.Timestamp, "INVALID_RELATIVE:")
				offsetSeconds, parseErr := strconv.ParseFloat(offsetStr, 64)
				if parseErr != nil {
					log.Printf("Failed to parse offset for event %d: %v", i, parseErr)
					continue
				}

				// Now that serviceStartTime is corrected, calculate absolute time
				correctedTime := s.serviceStartTime.Add(time.Duration(offsetSeconds * float64(time.Second)))
				event.Data.Timestamp = correctedTime.UTC().Format(time.RFC3339)
				log.Printf("Recalibrated event %d: offset %.3fs -> %s", i, offsetSeconds, event.Data.Timestamp)
			} else {
				// For absolute timestamps, apply the clock correction directly
				if eventTime, parseErr := time.Parse(time.RFC3339, event.Data.Timestamp); parseErr == nil {
					adjustedTime := eventTime.Add(clockCorrection)
					event.Data.Timestamp = adjustedTime.UTC().Format(time.RFC3339)
					log.Printf("Adjusted event %d timestamp by %.1f seconds -> %s", i, clockCorrection.Seconds(), event.Data.Timestamp)
				}
			}
		}
	}
	
	// Update last system time
	buffer.LastSystemTime = now

	// Increment sequence counter and add telemetry to buffer
	buffer.SequenceCounter++
	event := models.BufferedTelemetryEvent{
		Data:       data,
		Timestamp:  now,
		Attempts:   0,
		SequenceID: buffer.SequenceCounter,
	}
	buffer.Events = append(buffer.Events, event)

	// Check if buffer exceeds max size
	if len(buffer.Events) > s.config.Telemetry.Buffer.MaxSize {
		log.Printf("Buffer exceeds max size (%d), subsampling", s.config.Telemetry.Buffer.MaxSize)
		s.subsampleBuffer(buffer)
	}

	// Persist buffer to Redis first
	if err := s.saveBufferToRedis(buffer); err != nil {
		log.Printf("Warning: failed to persist buffer to Redis: %v", err)

		// Fall back to disk if Redis fails and disk is configured
		if s.config.Telemetry.Buffer.PersistPath != "" {
			if diskErr := s.saveBufferToDisk(buffer); diskErr != nil {
				log.Printf("Warning: failed to persist buffer to disk: %v", diskErr)
			}
		}
	}

	// Always succeed - telemetry is in memory even if persistence failed
	return nil
}

// subsampleBuffer reduces the size of the buffer by removing every 2nd event
func (s *ScooterMQTTClient) subsampleBuffer(buffer *models.TelemetryBuffer) {
	if len(buffer.Events) < 3 {
		return
	}

	first := buffer.Events[0]
	last := buffer.Events[len(buffer.Events)-1]

	newEvents := []models.BufferedTelemetryEvent{first}
	for i := 1; i < len(buffer.Events)-1; i += 2 {
		newEvents = append(newEvents, buffer.Events[i])
	}
	newEvents = append(newEvents, last)

	buffer.Events = newEvents
	log.Printf("Subsampled buffer from %d to %d events", len(buffer.Events), len(newEvents))
}

// resetBuffer clears the buffer and generates a new batch ID
func (s *ScooterMQTTClient) resetBuffer(buffer *models.TelemetryBuffer) {
	now := time.Now()
	buffer.Events = []models.BufferedTelemetryEvent{}
	batchID, err := generateRandomID()
	if err != nil {
		log.Printf("Failed to generate batch ID: %v", err)
		batchID = fmt.Sprintf("batch-%d", now.UnixNano())
	}
	buffer.BatchID = batchID
	buffer.CreatedAt = now
	buffer.SequenceCounter = 0
	buffer.LastSystemTime = now
}

// transmitBuffer transmits the telemetry buffer
func (s *ScooterMQTTClient) transmitBuffer() error {
	if !s.config.Telemetry.Buffer.Enabled {
		return nil
	}

	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()

	// Use in-memory buffer if available, otherwise load from persistence
	var buffer *models.TelemetryBuffer
	if s.buffer != nil {
		buffer = s.buffer
	} else {
		var err error
		buffer, err = s.loadBufferFromRedis()
		if err != nil {
			return fmt.Errorf("failed to load buffer from Redis: %v", err)
		}
		s.buffer = buffer
	}

	// If buffer is empty, return
	if len(buffer.Events) == 0 {
		return nil
	}

	// Create batch message
	batch := models.TelemetryBatch{
		BatchID:   buffer.BatchID,
		Count:     len(buffer.Events),
		Events:    make([]models.TelemetryData, len(buffer.Events)),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	// Copy events to batch
	for i, event := range buffer.Events {
		batch.Events[i] = *event.Data
	}

	// Marshal batch
	batchJSON, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("failed to marshal batch: %v", err)
	}

	// Publish batch
	topic := fmt.Sprintf("scooters/%s/telemetry_batch", s.config.Scooter.Identifier)
	if token := s.mqttClient.Publish(topic, 1, false, batchJSON); token.Wait() && token.Error() != nil {
		// Update attempt count for each event
		for i := range buffer.Events {
			buffer.Events[i].Attempts++
		}

		// Save buffer to Redis
		if err := s.saveBufferToRedis(buffer); err != nil {
			log.Printf("Failed to save buffer to Redis: %v", err)
		}

		return fmt.Errorf("failed to publish batch: %v", token.Error())
	}

	log.Printf("Published telemetry batch with %d events to %s", len(buffer.Events), topic)

	// Update cloud status (non-critical, just log if it fails)
	s.updateCloudStatus()

	// Transmission succeeded - reset in-memory buffer immediately
	// This prevents re-transmission in the current process run
	s.resetBuffer(buffer)

	// Try to persist the reset for durability across restarts
	if err := s.saveBufferToRedis(buffer); err != nil {
		log.Printf("Warning: failed to persist buffer reset to Redis: %v", err)

		// Fall back to disk if Redis fails and disk is configured
		if s.config.Telemetry.Buffer.PersistPath != "" {
			if diskErr := s.saveBufferToDisk(buffer); diskErr != nil {
				log.Printf("Warning: failed to persist buffer reset to disk: %v", diskErr)
				log.Printf("Buffer reset in memory only - may re-transmit on restart (batch ID: %s)", buffer.BatchID)
			} else {
				log.Printf("Buffer reset persisted to disk (Redis failed)")
			}
		} else {
			log.Printf("Buffer reset in memory only - may re-transmit on restart (batch ID: %s)", buffer.BatchID)
		}
	}

	// Always return success after MQTT transmission succeeds
	// In-memory reset prevents re-transmission during this process run
	return nil
}

// transmitBufferPeriodically transmits the telemetry buffer periodically
func (s *ScooterMQTTClient) transmitBufferPeriodically() {
	if !s.config.Telemetry.Buffer.Enabled {
		return
	}

	transmitPeriod, err := time.ParseDuration(s.config.Telemetry.TransmitPeriod)
	if err != nil {
		log.Printf("Failed to parse transmit period: %v, using default of 5m", err)
		transmitPeriod = 5 * time.Minute
	}

	ticker := time.NewTicker(transmitPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if err := s.transmitBuffer(); err != nil {
				log.Printf("Failed to transmit buffer: %v", err)
				go s.retryTransmitBuffer()
			}
		}
	}
}

// retryTransmitBuffer retries transmitting the buffer with exponential backoff
func (s *ScooterMQTTClient) retryTransmitBuffer() {
	if !s.config.Telemetry.Buffer.Enabled {
		return
	}

	s.bufferMu.Lock()
	buffer, err := s.loadBufferFromRedis()
	if err != nil {
		s.bufferMu.Unlock()
		log.Printf("Failed to load buffer from Redis: %v", err)
		return
	}

	if len(buffer.Events) == 0 {
		s.bufferMu.Unlock()
		return
	}

	maxRetries := s.config.Telemetry.Buffer.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	retryIntervalStr := s.config.Telemetry.Buffer.RetryInterval
	if retryIntervalStr == "" {
		retryIntervalStr = "1m"
	}
	retryInterval, err := time.ParseDuration(retryIntervalStr)
	if err != nil {
		log.Printf("Failed to parse retry interval: %v, using default of 1m", err)
		retryInterval = time.Minute
	}

	maxAttempts := 0
	for _, event := range buffer.Events {
		if event.Attempts > maxAttempts {
			maxAttempts = event.Attempts
		}
	}

	if maxAttempts >= maxRetries {
		log.Printf("Max retries exceeded (%d), dropping %d events", maxRetries, len(buffer.Events))
		s.resetBuffer(buffer)

		if err := s.saveBufferToRedis(buffer); err != nil {
			log.Printf("Failed to save buffer to Redis: %v", err)
		}
		s.bufferMu.Unlock()
		return
	}
	s.bufferMu.Unlock()

	backoff := calculateBackoff(maxAttempts, retryInterval)
	log.Printf("Retrying buffer transmission in %v (attempt %d/%d)", backoff, maxAttempts+1, maxRetries)

	select {
	case <-s.ctx.Done():
		return
	case <-time.After(backoff):
		if err := s.transmitBuffer(); err != nil {
			log.Printf("Failed to transmit buffer: %v", err)
			if s.ctx.Err() == nil {
				go s.retryTransmitBuffer()
			}
		}
	}
}

// calculateBackoff calculates the backoff time based on attempt count
func calculateBackoff(attempt int, baseInterval time.Duration) time.Duration {
	// Calculate backoff with exponential increase and jitter
	backoff := float64(baseInterval) * math.Pow(2, float64(attempt))
	// Add jitter (±20%)
	jitter := (rand.Float64()*0.4 - 0.2) * backoff
	backoff += jitter
	return time.Duration(backoff)
}

// generateRandomID generates a random ID for batch identification
func generateRandomID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := cryptorand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// collectAndPublishTelemetry collects telemetry data and publishes it
func (s *ScooterMQTTClient) collectAndPublishTelemetry() error {
	current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version, s.serviceStartTime)
	if err != nil {
		return fmt.Errorf("failed to get telemetry: %v", err)
	}

	if s.config.Telemetry.Buffer.Enabled {
		if err := s.addTelemetryToBuffer(current); err != nil {
			log.Printf("Failed to add telemetry to buffer: %v", err)
		}
		return nil
	}

	return s.publishTelemetryData(current)
}
