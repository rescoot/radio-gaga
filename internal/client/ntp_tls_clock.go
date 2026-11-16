package client

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"radio-gaga/internal/models"
	"radio-gaga/internal/utils"
)

var queryTLSTime = utils.QueryNTPTime

type ntpTLSClock struct {
	mu        sync.Mutex
	config    *models.NTPConfig
	reference time.Time
	received  int64
}

func (c *ntpTLSClock) current() (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now); err != nil {
		return time.Time{}, err
	}
	elapsed := now.Nano() - c.received
	if c.received != 0 && elapsed >= 0 && elapsed < int64(30*time.Second) {
		return c.reference.Add(time.Duration(elapsed)), nil
	}
	reference, err := queryTLSTime(c.config)
	if err != nil {
		return time.Time{}, err
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &now); err != nil {
		return time.Time{}, err
	}
	c.reference, c.received = reference, now.Nano()
	return reference, nil
}

func (c *ntpTLSClock) time() time.Time {
	now, err := c.current()
	if err != nil {
		// X.509 certificate dates cannot extend beyond year 9999.
		return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return now
}
