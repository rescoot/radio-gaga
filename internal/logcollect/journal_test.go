package logcollect

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJournalWindow(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  time.Duration
	}{
		{"1h", time.Hour}, {" 15m ", 15 * time.Minute},
		{"24h", 24 * time.Hour}, {"2d", 48 * time.Hour},
		{"1w", 7 * 24 * time.Hour}, {"1h30m", 90 * time.Minute},
		{"", 0}, {"0h", 0}, {"-1h", 0}, {"-1d", 0},
		{"9999999999999999999w", 0}, {"today", 0},
		{"2026-10-29 12:00", 0},
	} {
		t.Run(tt.input, func(t *testing.T) {
			got, ok := journalWindow(tt.input)
			if got != tt.want || ok != (tt.want > 0) {
				t.Fatalf("journalWindow(%q) = %v, %v; want %v", tt.input, got, ok, tt.want)
			}
		})
	}
}

func TestYoungBootSurvivesClockCorrectionWithoutDuplicates(t *testing.T) {
	now := time.Unix(1793275200, 123456789)
	cutoff := now.Add(-time.Hour).Truncate(time.Microsecond)
	commands := journalCommands("1h", 165, now)
	if len(commands) != 2 {
		t.Fatal("young boot needs both its missing startup and the requested history")
	}
	type entry struct {
		name    string
		current bool
		at      time.Time
	}
	entries := []entry{
		{"old boot outside window", false, now.Add(-8 * time.Hour)},
		{"startup before clock sync", true, now.Add(-7 * time.Hour)},
		{"previous boot within window", false, cutoff.Add(time.Second)},
		{"last excluded microsecond", true, cutoff.Add(-time.Microsecond)},
		{"exact boundary", true, cutoff},
		{"after clock sync", true, now.Add(-time.Minute)},
	}
	counts := map[string]int{}
	for _, args := range commands {
		var since, until time.Time
		currentOnly := false
		for i, arg := range args {
			switch arg {
			case "--boot=0":
				currentOnly = true
			case "--since", "--until":
				var sec, us int64
				if _, err := fmt.Sscanf(args[i+1], "@%d.%d", &sec, &us); err != nil {
					t.Fatal(err)
				}
				at := time.Unix(sec, us*1000)
				if arg == "--since" {
					since = at
				} else {
					until = at
				}
			}
		}
		for _, e := range entries {
			if currentOnly && !e.current || !since.IsZero() && e.at.Before(since) || !until.IsZero() && e.at.After(until) {
				continue
			}
			counts[e.name]++
		}
	}
	for _, e := range entries {
		want := 1
		if e.name == "old boot outside window" {
			want = 0
		}
		if counts[e.name] != want {
			t.Errorf("%s captured %d times, want %d", e.name, counts[e.name], want)
		}
	}
}

func TestJournalCommandsKeepWindowForOldOrUnknownBoot(t *testing.T) {
	for _, uptime := range []float64{0, -1, 3600.001, 864000} {
		got := journalCommands("1h", uptime, time.Now())
		want := [][]string{{"-o", "short-monotonic", "--no-pager", "--since", "1 hour ago"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("uptime %v expanded the requested window: %v", uptime, got)
		}
	}
	if len(journalCommands("1h", 3600, time.Now())) != 2 {
		t.Fatal("boot exactly at the duration boundary must be included")
	}
	for _, since := range []string{"today", "2026-10-29 12:00", ""} {
		commands := journalCommands(since, 10, time.Now())
		if len(commands) != 1 || strings.Contains(strings.Join(commands[0], " "), "--boot") {
			t.Fatalf("native journal window %q was changed: %v", since, commands)
		}
	}
}

func fakeJournal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("CALLS", calls)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	journal := `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  *--boot=0*)
    test "$FAIL_BOOT" != 1 || exit 7
    printf '[    6.000000] dbc scootui-qt[300]: startup before clock sync\n'
    ;;
  *) printf '[   24.000000] dbc chronyd[500]: clock synchronized\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(journal), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec sh -c \"$3\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return calls
}

func TestBothCollectorsIncludeBoot(t *testing.T) {
	for name, run := range map[string]func(context.Context, string, string, float64) (int64, error){
		"mdb": runJournalctl, "dbc": runDBCJournalctl,
	} {
		t.Run(name, func(t *testing.T) {
			calls := fakeJournal(t)
			out := filepath.Join(t.TempDir(), "journal.log")
			n, err := run(context.Background(), "1h", out, 165)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(data)) || !strings.Contains(string(data), "startup before clock sync") || !strings.Contains(string(data), "clock synchronized") {
				t.Fatalf("incomplete journal (%d bytes): %q", n, data)
			}
			invocations, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(invocations), "\n") != 2 {
				t.Fatalf("unexpected journal invocations: %q", invocations)
			}
		})
	}
}

func TestBothCollectorsPropagateBootQueryFailure(t *testing.T) {
	for name, run := range map[string]func(context.Context, string, string, float64) (int64, error){
		"mdb": runJournalctl, "dbc": runDBCJournalctl,
	} {
		t.Run(name, func(t *testing.T) {
			calls := fakeJournal(t)
			t.Setenv("FAIL_BOOT", "1")
			_, err := run(context.Background(), "1h", filepath.Join(t.TempDir(), "journal.log"), 165)
			if err == nil {
				t.Fatal("failed boot query reported success")
			}
			invocations, _ := os.ReadFile(calls)
			if strings.Count(string(invocations), "\n") != 1 {
				t.Fatalf("continued after failure: %q", invocations)
			}
		})
	}
}

func TestRemoteJournalCommandQuotesNativeWindow(t *testing.T) {
	since := "today'; touch /tmp/not-a-command; '"
	commands := journalCommands(since, 10, time.Now())
	remote := remoteJournalCommand(commands)
	if !strings.HasSuffix(remote, shellQuote(since)) || strings.Contains(remote, " && ") {
		t.Fatalf("unsafe remote command: %s", remote)
	}
}
