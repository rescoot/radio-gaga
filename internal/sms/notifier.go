package sms

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"radio-gaga/internal/events"
	"radio-gaga/internal/models"
)

// Notifier sends SMS notifications via mmcli (ModemManager CLI)
type Notifier struct {
	config     *models.SMSConfig
	identifier string
	name       string
	queue      chan events.Event
	rateLimit  time.Duration
	cancel     context.CancelFunc
	wg         sync.WaitGroup

	// Daily limit tracking (in-memory, resets on restart)
	dailyMu    sync.Mutex
	dailyCount int
	dailyDate  string // "YYYY-MM-DD"
}

// NewNotifier creates a new SMS notifier
func NewNotifier(config *models.SMSConfig, scooterConfig *models.ScooterConfig) (*Notifier, error) {
	rateLimit, err := time.ParseDuration(config.RateLimit)
	if err != nil {
		return nil, fmt.Errorf("invalid sms rate_limit: %v", err)
	}

	queueSize := config.QueueSize
	if queueSize <= 0 {
		queueSize = 20
	}

	return &Notifier{
		config:     config,
		identifier: scooterConfig.Identifier,
		name:       scooterConfig.Name,
		queue:      make(chan events.Event, queueSize),
		rateLimit:  rateLimit,
	}, nil
}

// ChannelName returns the channel identifier for this notifier
func (n *Notifier) ChannelName() string {
	return "sms"
}

// Start begins processing the notification queue
func (n *Notifier) Start(ctx context.Context) {
	ctx, n.cancel = context.WithCancel(ctx)
	n.wg.Add(1)
	go n.processQueue(ctx)
	log.Printf("[SMS] Notifier started (rate_limit=%s, queue_size=%d)", n.rateLimit, cap(n.queue))
}

// Stop gracefully shuts down the notifier
func (n *Notifier) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	n.wg.Wait()
	log.Println("[SMS] Notifier stopped")
}

// Notify enqueues an event for sending (non-blocking)
func (n *Notifier) Notify(event events.Event) {
	select {
	case n.queue <- event:
	default:
		log.Printf("[SMS] Queue full, dropping event: %s", event.EventType)
	}
}

// ShouldNotify always returns false — SMS is only triggered via explicit channel routing in rules
func (n *Notifier) ShouldNotify(_ string) bool {
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
			if err := n.sendSMS(event); err != nil {
				log.Printf("[SMS] Failed to send: %v", err)
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

// checkDailyLimit returns true if a message can be sent (under the daily limit)
func (n *Notifier) checkDailyLimit() bool {
	if n.config.DailyLimit <= 0 {
		return true
	}
	today := time.Now().Format("2006-01-02")
	n.dailyMu.Lock()
	defer n.dailyMu.Unlock()
	if n.dailyDate != today {
		n.dailyDate = today
		n.dailyCount = 0
	}
	if n.dailyCount >= n.config.DailyLimit {
		return false
	}
	n.dailyCount++
	return true
}

func (n *Notifier) sendSMS(event events.Event) error {
	if !n.checkDailyLimit() {
		log.Printf("[SMS] Daily limit (%d) reached, dropping event: %s", n.config.DailyLimit, event.EventType)
		return nil
	}

	text := n.formatMessage(event)
	number := n.config.PhoneNumber

	// mmcli's property parser splits on commas but respects quoted values.
	// It has no escape mechanism (uses strchr for the closing quote), so we
	// pick whichever quote type doesn't appear in the text.
	arg := formatMMCLICreateArg(number, text)

	createOut, err := exec.Command("mmcli", "-m", "0", arg).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mmcli create failed: %v (output: %s)", err, string(createOut))
	}

	// Extract the SMS path from output, e.g. "/org/freedesktop/ModemManager1/SMS/0"
	smsPath := extractSMSPath(string(createOut))
	if smsPath == "" {
		return fmt.Errorf("could not parse SMS path from mmcli output: %s", string(createOut))
	}

	// Send the SMS
	if sendOut, err := exec.Command("mmcli", "-s", smsPath, "--send").CombinedOutput(); err != nil {
		return fmt.Errorf("mmcli send failed: %v (output: %s)", err, string(sendOut))
	}

	log.Printf("[SMS] Sent %s notification to %s", event.EventType, number)
	return nil
}

// formatMMCLICreateArg builds the --messaging-create-sms argument with safe quoting.
// mmcli's property parser respects single and double quotes but has no escape mechanism,
// so we pick whichever quote type doesn't appear in the text.
func formatMMCLICreateArg(number, text string) string {
	q := `"`
	if strings.Contains(text, `"`) {
		q = "'"
		if strings.Contains(text, "'") {
			// Both quote types present — strip double quotes (less common in alerts)
			text = strings.ReplaceAll(text, `"`, "")
			q = `"`
		}
	}
	return fmt.Sprintf("--messaging-create-sms=number=%s%s%s,text=%s%s%s", q, number, q, q, text, q)
}

// extractSMSPath extracts the D-Bus path from mmcli --messaging-create-sms output
func extractSMSPath(output string) string {
	// mmcli output looks like:
	// Successfully created new SMS: /org/freedesktop/ModemManager1/SMS/0
	for _, line := range strings.Split(output, "\n") {
		if idx := strings.Index(line, "/org/freedesktop/ModemManager1/SMS/"); idx >= 0 {
			path := line[idx:]
			// Trim trailing whitespace/newline
			return strings.TrimSpace(path)
		}
	}
	return ""
}

// formatMessage formats an event into a plain-text SMS message
func (n *Notifier) formatMessage(event events.Event) string {
	scooter := n.name
	if scooter == "" {
		scooter = n.identifier
	}

	d := event.Data
	str := func(key string) string {
		if d == nil {
			return ""
		}
		if v, ok := d[key]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}

	switch event.EventType {
	case events.EventTypeAlarm:
		return fmt.Sprintf("[%s] alarm: %s", scooter, event.Status)

	case events.EventTypeUnauthorizedMovement:
		msg := fmt.Sprintf("[%s] moving while parked", scooter)
		if s := str("gps_speed"); s != "" {
			msg += fmt.Sprintf(" (%s km/h)", s)
		}
		return msg

	case events.EventTypeBatteryWarning:
		return fmt.Sprintf("[%s] battery %s at %s%%", scooter, str("battery"), str("charge"))

	case events.EventTypeTemperatureWarning:
		return fmt.Sprintf("[%s] %s temp at %s°C", scooter, str("component"), str("temperature"))

	case events.EventTypeNotificationRule:
		rule := str("rule")
		if msg := str("message"); msg != "" {
			return fmt.Sprintf("[%s] %s", scooter, msg)
		}
		if condRaw, ok := d["conditions"]; ok && condRaw != nil {
			if conds, ok := condRaw.(map[string]string); ok && len(conds) > 0 {
				for k, v := range conds {
					return fmt.Sprintf("[%s] rule %s: %s=%s", scooter, rule, k, v)
				}
			}
		}
		return fmt.Sprintf("[%s] rule %s triggered", scooter, rule)

	default:
		if event.Status != "" {
			return fmt.Sprintf("[%s] %s: %s", scooter, event.EventType, event.Status)
		}
		return fmt.Sprintf("[%s] %s", scooter, event.EventType)
	}
}
