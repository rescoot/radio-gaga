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
	"sync"
	"time"

	"radio-gaga/internal/models"
	"radio-gaga/internal/telemetry"
)

const (
	// Redis key for storing the telemetry buffer
	telemetryBufferKey = "radio-gaga:telemetry-buffer"
)

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

	// Save buffer to Redis
	if err := s.saveBufferToRedis(buffer); err != nil {
		log.Printf("Failed to save buffer to Redis: %v", err)
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

	return &models.TelemetryBuffer{
		Events:    []models.BufferedTelemetryEvent{},
		BatchID:   batchID,
		CreatedAt: time.Now(),
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

	// Create directory if it doesn't exist
	dir := s.config.Telemetry.Buffer.PersistPath
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %v", err)
		}
	}

	// Write buffer to disk
	if err := os.WriteFile(s.config.Telemetry.Buffer.PersistPath, bufferJSON, 0644); err != nil {
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

	// Load buffer from Redis
	buffer, err := s.loadBufferFromRedis()
	if err != nil {
		log.Printf("Failed to load buffer from Redis: %v", err)
		// Create a new buffer
		buffer = s.createNewBuffer()
	}

	// Add telemetry to buffer
	event := models.BufferedTelemetryEvent{
		Data:      data,
		Timestamp: time.Now(),
		Attempts:  0,
	}
	buffer.Events = append(buffer.Events, event)

	// Check if buffer exceeds max size
	if len(buffer.Events) > s.config.Telemetry.Buffer.MaxSize {
		log.Printf("Buffer exceeds max size (%d), subsampling", s.config.Telemetry.Buffer.MaxSize)
		s.subsampleBuffer(buffer)
	}

	// Save buffer to Redis
	if err := s.saveBufferToRedis(buffer); err != nil {
		return fmt.Errorf("failed to save buffer to Redis: %v", err)
	}

	// Save buffer to disk if persist path is set
	if s.config.Telemetry.Buffer.PersistPath != "" {
		if err := s.saveBufferToDisk(buffer); err != nil {
			log.Printf("Failed to save buffer to disk: %v", err)
		}
	}

	return nil
}

// subsampleBuffer reduces the size of the buffer by removing every 2nd event
func (s *ScooterMQTTClient) subsampleBuffer(buffer *models.TelemetryBuffer) {
	// If buffer has less than 3 events, don't subsample
	if len(buffer.Events) < 3 {
		return
	}

	// Keep first and last events
	first := buffer.Events[0]
	last := buffer.Events[len(buffer.Events)-1]

	// Remove every 2nd event in between
	newEvents := []models.BufferedTelemetryEvent{first}
	for i := 1; i < len(buffer.Events)-1; i += 2 {
		newEvents = append(newEvents, buffer.Events[i])
	}
	newEvents = append(newEvents, last)

	// Update buffer
	buffer.Events = newEvents
	log.Printf("Subsampled buffer from %d to %d events", len(buffer.Events), len(newEvents))
}

// transmitBuffer transmits the telemetry buffer
func (s *ScooterMQTTClient) transmitBuffer() error {
	// If buffer is not enabled, return
	if !s.config.Telemetry.Buffer.Enabled {
		return nil
	}

	// Load buffer from Redis
	buffer, err := s.loadBufferFromRedis()
	if err != nil {
		return fmt.Errorf("failed to load buffer from Redis: %v", err)
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

	// Update cloud status since we successfully published to MQTT
	s.updateCloudStatus()

	// Clear buffer
	buffer.Events = []models.BufferedTelemetryEvent{}
	buffer.BatchID, _ = generateRandomID()
	buffer.CreatedAt = time.Now()

	// Save buffer to Redis
	if err := s.saveBufferToRedis(buffer); err != nil {
		return fmt.Errorf("failed to save buffer to Redis: %v", err)
	}

	// Save buffer to disk if persist path is set
	if s.config.Telemetry.Buffer.PersistPath != "" {
		if err := s.saveBufferToDisk(buffer); err != nil {
			log.Printf("Failed to save buffer to disk: %v", err)
		}
	}

	return nil
}

// transmitBufferPeriodically transmits the telemetry buffer periodically
func (s *ScooterMQTTClient) transmitBufferPeriodically() {
	// If buffer is not enabled, return
	if !s.config.Telemetry.Buffer.Enabled {
		return
	}

	// Parse transmit period
	transmitPeriod, err := time.ParseDuration(s.config.Telemetry.TransmitPeriod)
	if err != nil {
		log.Printf("Failed to parse transmit period: %v, using default of 5m", err)
		transmitPeriod = 5 * time.Minute
	}

	// Create ticker
	ticker := time.NewTicker(transmitPeriod)
	defer ticker.Stop()

	// Create mutex for buffer access
	var mutex sync.Mutex

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Lock mutex
			mutex.Lock()

			// Transmit buffer
			if err := s.transmitBuffer(); err != nil {
				log.Printf("Failed to transmit buffer: %v", err)
				// Retry with backoff
				go s.retryTransmitBuffer(&mutex)
			}

			// Unlock mutex
			mutex.Unlock()
		}
	}
}

// retryTransmitBuffer retries transmitting the buffer with exponential backoff
func (s *ScooterMQTTClient) retryTransmitBuffer(mutex *sync.Mutex) {
	// If buffer is not enabled, return
	if !s.config.Telemetry.Buffer.Enabled {
		return
	}

	// Load buffer from Redis
	buffer, err := s.loadBufferFromRedis()
	if err != nil {
		log.Printf("Failed to load buffer from Redis: %v", err)
		return
	}

	// If buffer is empty, return
	if len(buffer.Events) == 0 {
		return
	}

	// Get max retries
	maxRetries := s.config.Telemetry.Buffer.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	// Get retry interval
	retryIntervalStr := s.config.Telemetry.Buffer.RetryInterval
	if retryIntervalStr == "" {
		retryIntervalStr = "1m"
	}
	retryInterval, err := time.ParseDuration(retryIntervalStr)
	if err != nil {
		log.Printf("Failed to parse retry interval: %v, using default of 1m", err)
		retryInterval = time.Minute
	}

	// Check if any event has exceeded max retries
	maxAttempts := 0
	for _, event := range buffer.Events {
		if event.Attempts > maxAttempts {
			maxAttempts = event.Attempts
		}
	}

	// If max retries exceeded, log and return
	if maxAttempts >= maxRetries {
		log.Printf("Max retries exceeded (%d), dropping %d events", maxRetries, len(buffer.Events))
		// Clear buffer
		buffer.Events = []models.BufferedTelemetryEvent{}
		buffer.BatchID, _ = generateRandomID()
		buffer.CreatedAt = time.Now()

		// Save buffer to Redis
		if err := s.saveBufferToRedis(buffer); err != nil {
			log.Printf("Failed to save buffer to Redis: %v", err)
		}
		return
	}

	// Calculate backoff time
	backoff := calculateBackoff(maxAttempts, retryInterval)
	log.Printf("Retrying buffer transmission in %v (attempt %d/%d)", backoff, maxAttempts+1, maxRetries)

	// Wait for backoff time
	select {
	case <-s.ctx.Done():
		return
	case <-time.After(backoff):
		// Lock mutex
		mutex.Lock()
		defer mutex.Unlock()

		// Transmit buffer
		if err := s.transmitBuffer(); err != nil {
			log.Printf("Failed to transmit buffer: %v", err)
			// Retry again if not canceled
			if s.ctx.Err() == nil {
				go s.retryTransmitBuffer(mutex)
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
	// Get telemetry data
	current, err := telemetry.GetTelemetryFromRedis(s.ctx, s.redisClient, s.config, s.version)
	if err != nil {
		return fmt.Errorf("failed to get telemetry: %v", err)
	}

	// If buffer is enabled, add to buffer
	if s.config.Telemetry.Buffer.Enabled {
		if err := s.addTelemetryToBuffer(current); err != nil {
			log.Printf("Failed to add telemetry to buffer: %v", err)
		}
		return nil
	}

	// Otherwise, publish directly
	return s.publishTelemetryData(current)
}
