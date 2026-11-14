package sms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"radio-gaga/internal/events"
	"radio-gaga/internal/models"
)

func setupMMCLI(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	list := filepath.Join(dir, "list")
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MMCLI_LOG"
case "$1 $2" in
  '-J -L') if [ "$MMCLI_FAIL_LIST" = 1 ]; then echo 'list failed' >&2; exit 1; fi; /bin/cat "$MMCLI_LIST" ;;
  '-m '*) echo 'Successfully created new SMS: /org/freedesktop/ModemManager1/SMS/4' ;;
  '-s '*) echo sent ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "mmcli"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("MMCLI_LIST", list)
	t.Setenv("MMCLI_LOG", log)
	return list, log
}

func TestSendSMSResolvesModemForEachSend(t *testing.T) {
	list, log := setupMMCLI(t)
	n := &Notifier{config: &models.SMSConfig{PhoneNumber: "+123"}, name: "scooter"}
	for _, id := range []string{"7", "12"} {
		data := `{"modem-list":["/org/freedesktop/ModemManager1/Modem/` + id + `"]}`
		if err := os.WriteFile(list, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := n.sendSMS(events.Event{EventType: events.EventTypeAlarm}); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-m /org/freedesktop/ModemManager1/Modem/7 ", "-m /org/freedesktop/ModemManager1/Modem/12 "} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("missing %q in calls: %s", want, calls)
		}
	}
	if strings.Count(string(calls), "-s /org/freedesktop/ModemManager1/SMS/4 --send") != 2 {
		t.Errorf("expected two sends, calls: %s", calls)
	}
}

func TestSendSMSRejectsInvalidModemLists(t *testing.T) {
	for _, tc := range []struct {
		name, list, want string
		fail             bool
	}{
		{"none", `{"modem-list":[]}`, "found 0", false},
		{"multiple", `{"modem-list":["/org/freedesktop/ModemManager1/Modem/1","/org/freedesktop/ModemManager1/Modem/2"]}`, "found 2", false},
		{"invalid path", `{"modem-list":["/bad/1"]}`, "invalid modem path", false},
		{"invalid json", `not json`, "parse mmcli modem list", false},
		{"list failure", ``, "list modems failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, log := setupMMCLI(t)
			if err := os.WriteFile(list, []byte(tc.list), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				t.Setenv("MMCLI_FAIL_LIST", "1")
			}
			n := &Notifier{config: &models.SMSConfig{}}
			if err := n.sendSMS(events.Event{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
			calls, err := os.ReadFile(log)
			if err != nil || string(calls) != "-J -L\n" {
				t.Fatalf("unexpected calls %q, error %v", calls, err)
			}
		})
	}
}

func TestNewNotifierRejectsNonPositiveRateLimit(t *testing.T) {
	for _, value := range []string{"0s", "-1s"} {
		cfg := &models.SMSConfig{RateLimit: value}
		if _, err := NewNotifier(cfg, &models.ScooterConfig{}); err == nil {
			t.Fatalf("accepted rate_limit %q", value)
		}
	}
}
