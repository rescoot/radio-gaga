package telegram

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"radio-gaga/internal/events"
	"radio-gaga/internal/models"
)

// Notifier sends Telegram notifications for scooter events
type Notifier struct {
	config     *models.TelegramConfig
	identifier string
	name       string
	queue      chan events.Event
	client     *http.Client
	rateLimit  time.Duration
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// NewNotifier creates a new Telegram notifier
func NewNotifier(config *models.TelegramConfig, scooterConfig *models.ScooterConfig) (*Notifier, error) {
	rateLimit, err := time.ParseDuration(config.RateLimit)
	if err != nil {
		return nil, fmt.Errorf("invalid rate_limit: %v", err)
	}

	return &Notifier{
		config:     config,
		identifier: scooterConfig.Identifier,
		name:       scooterConfig.Name,
		queue:      make(chan events.Event, config.QueueSize),
		client:     &http.Client{Timeout: 10 * time.Second},
		rateLimit:  rateLimit,
	}, nil
}

// Start begins processing the notification queue
func (n *Notifier) Start(ctx context.Context) {
	ctx, n.cancel = context.WithCancel(ctx)
	n.wg.Add(1)
	go n.processQueue(ctx)
	log.Printf("[Telegram] Notifier started (rate_limit=%s, queue_size=%d)", n.rateLimit, n.config.QueueSize)
}

// Stop gracefully shuts down the notifier
func (n *Notifier) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
	log.Println("[Telegram] Notifier stopped")
}

// Notify enqueues an event for sending (non-blocking)
func (n *Notifier) Notify(event events.Event) {
	select {
	case n.queue <- event:
	default:
		log.Printf("[Telegram] Queue full, dropping event: %s", event.EventType)
	}
}

// ShouldNotify checks whether notifications are enabled for an event type
func (n *Notifier) ShouldNotify(eventType string) bool {
	if enabled, ok := n.config.Events[eventType]; ok {
		return enabled
	}
	return false
}

func (n *Notifier) processQueue(ctx context.Context) {
	defer n.wg.Done()
	ticker := time.NewTicker(n.rateLimit)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-n.queue:
			if err := n.sendToTelegram(event); err != nil {
				log.Printf("[Telegram] Failed to send: %v", err)
			}
			// Rate limit: wait for next tick before processing more
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

func (n *Notifier) sendToTelegram(event events.Event) error {
	text := n.FormatMessage(event)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.config.BotToken)

	resp, err := n.client.PostForm(apiURL, url.Values{
		"chat_id":    {n.config.ChatID},
		"text":       {text},
		"parse_mode": {"HTML"},
	})
	if err != nil {
		return fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram API returned status %d", resp.StatusCode)
	}

	log.Printf("[Telegram] Sent %s notification", event.EventType)
	return nil
}

// FormatMessage formats an event into a concise one-line style message for Telegram
func (n *Notifier) FormatMessage(event events.Event) string {
	scooter := n.name
	if scooter == "" {
		scooter = n.identifier
	}

	d := event.Data
	str := func(key string) string {
		if v, ok := d[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}

	switch event.EventType {
	case events.EventTypeAlarm:
		switch event.Status {
		case "armed":
			return fmt.Sprintf("🔒 %s alarm armed", scooter)
		case "disarmed":
			return fmt.Sprintf("🔓 %s alarm disarmed", scooter)
		case "triggered":
			return fmt.Sprintf("🚨 %s alarm triggered!", scooter)
		default:
			return fmt.Sprintf("🚨 %s alarm: %s", scooter, event.Status)
		}

	case events.EventTypeUnauthorizedMovement:
		msg := fmt.Sprintf("⚠️ %s is moving while parked", scooter)
		if s := str("gps_speed"); s != "" {
			msg += fmt.Sprintf(" (%s km/h)", s)
		}
		return msg

	case events.EventTypeBatteryWarning:
		battery := formatBatteryName(str("battery"))
		charge := str("charge")
		return fmt.Sprintf("🔋 %s %s at %s%%", scooter, battery, charge)

	case events.EventTypeTemperatureWarning:
		component := formatComponentName(str("component"))
		temp := str("temperature")
		return fmt.Sprintf("🌡️ %s %s at %s°C", scooter, component, temp)

	case events.EventTypeStateChange:
		return fmt.Sprintf("🔄 %s: %s → %s", scooter, str("from"), str("to"))

	case events.EventTypeConnectivity:
		if event.Status == events.StatusLost {
			return fmt.Sprintf("📡 %s lost internet", scooter)
		}
		return fmt.Sprintf("📡 %s back online", scooter)

	case events.EventTypeFault:
		if str("type") == "nrf_reset" {
			return fmt.Sprintf("⚙️ %s NRF reset (reason: %s, count: %s)", scooter, str("reason"), str("count"))
		}
		desc := str("description")
		if desc != "" {
			return fmt.Sprintf("⚙️ %s fault: %s", scooter, desc)
		}
		return fmt.Sprintf("⚙️ %s fault: group %s, code %s", scooter, str("group"), str("code"))

	default:
		if event.Status != "" {
			return fmt.Sprintf("📢 %s %s: %s", scooter, event.EventType, event.Status)
		}
		return fmt.Sprintf("📢 %s %s", scooter, event.EventType)
	}
}

func formatBatteryName(key string) string {
	switch key {
	case "battery:0":
		return "Battery 1"
	case "battery:1":
		return "Battery 2"
	case "cb-battery":
		return "CB battery"
	default:
		return key
	}
}

func formatComponentName(key string) string {
	switch key {
	case "engine-ecu":
		return "Engine"
	case "battery:0":
		return "Battery 1"
	case "battery:1":
		return "Battery 2"
	default:
		return key
	}
}
