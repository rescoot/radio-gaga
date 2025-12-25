package events

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Buffer stores events for later transmission when disconnected
type Buffer struct {
	path       string
	maxRetries int

	mu     sync.Mutex
	events []BufferedEvent
}

// NewBuffer creates a new event buffer
func NewBuffer(path string, maxRetries int) *Buffer {
	b := &Buffer{
		path:       path,
		maxRetries: maxRetries,
		events:     make([]BufferedEvent, 0),
	}

	// Load any existing buffered events from disk
	b.loadFromDisk()

	return b
}

// Add adds an event to the buffer and persists to disk
func (b *Buffer) Add(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	buffered := BufferedEvent{
		Event:   event,
		Retries: 0,
	}

	b.events = append(b.events, buffered)
	b.persistToDisk()

	log.Printf("[EventBuffer] Buffered event %s (total: %d)", event.EventType, len(b.events))
}

// Flush attempts to send all buffered events using the provided sender function
func (b *Buffer) Flush(sender func(Event) error) {
	b.mu.Lock()
	if len(b.events) == 0 {
		b.mu.Unlock()
		return
	}

	log.Printf("[EventBuffer] Flushing %d buffered events...", len(b.events))
	events := make([]BufferedEvent, len(b.events))
	copy(events, b.events)
	b.mu.Unlock()

	var failed []BufferedEvent
	successCount := 0
	discardedCount := 0

	for _, buffered := range events {
		// Check if exceeded max retries
		if buffered.Retries >= b.maxRetries {
			log.Printf("[EventBuffer] Event %s exceeded max retries (%d), discarding",
				buffered.Event.EventType, b.maxRetries)
			discardedCount++
			continue
		}

		// Try to send
		if err := sender(buffered.Event); err != nil {
			log.Printf("[EventBuffer] Failed to send event %s (retry %d/%d): %v",
				buffered.Event.EventType, buffered.Retries+1, b.maxRetries, err)

			// Increment retry count
			buffered.Retries++
			failed = append(failed, buffered)

			// Exponential backoff delay
			backoff := time.Duration(1<<uint(buffered.Retries-1)) * time.Second
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			time.Sleep(backoff)
			continue
		}

		successCount++
		// Small delay between successful sends to avoid overwhelming the server
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("[EventBuffer] Flushed %d events, %d failed (will retry), %d discarded",
		successCount, len(failed), discardedCount)

	// Update buffer with only failed events
	b.mu.Lock()
	b.events = failed
	b.persistToDisk()
	b.mu.Unlock()
}

// Count returns the number of buffered events
func (b *Buffer) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// persistToDisk writes the buffer to disk
// Must be called with mutex held
func (b *Buffer) persistToDisk() {
	if b.path == "" {
		return
	}

	// Ensure directory exists
	dir := filepath.Dir(b.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[EventBuffer] Failed to create directory %s: %v", dir, err)
		return
	}

	// If no events, remove the file
	if len(b.events) == 0 {
		os.Remove(b.path)
		return
	}

	// Write events as JSON lines
	f, err := os.Create(b.path)
	if err != nil {
		log.Printf("[EventBuffer] Failed to create buffer file: %v", err)
		return
	}
	defer f.Close()

	for _, event := range b.events {
		data, err := json.Marshal(event)
		if err != nil {
			log.Printf("[EventBuffer] Failed to marshal event: %v", err)
			continue
		}
		f.Write(data)
		f.Write([]byte("\n"))
	}
}

// loadFromDisk loads buffered events from disk
func (b *Buffer) loadFromDisk() {
	if b.path == "" {
		return
	}

	f, err := os.Open(b.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[EventBuffer] Failed to open buffer file: %v", err)
		}
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var buffered BufferedEvent
		if err := json.Unmarshal([]byte(line), &buffered); err != nil {
			log.Printf("[EventBuffer] Failed to parse buffered event: %v", err)
			continue
		}

		b.events = append(b.events, buffered)
		count++
	}

	if count > 0 {
		log.Printf("[EventBuffer] Loaded %d buffered events from disk", count)
	}
}
