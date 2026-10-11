package cellular

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"radio-gaga/internal/models"
)

func TestValidate(t *testing.T) {
	for _, auth := range []string{"", "none", "pap", "chap"} {
		if err := Validate(models.CellularConfig{Auth: auth}); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []models.CellularConfig{
		{Auth: "auto"}, {APN: "bad\"apn"}, {Username: "u\rAT"},
		{Password: "p\nAT"}, {Password: "p,apn=other"}, {Password: "p'"}, {APN: "a\x00"},
	} {
		if err := Validate(cfg); err == nil {
			t.Fatalf("accepted unsafe configuration")
		}
	}
}

func TestSeedSettingsPreservesValuesAndPublishesFields(t *testing.T) {
	m := miniredis.RunT(t)
	r := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer r.Close()
	ctx := context.Background()
	if err := r.HSet(ctx, "settings", "cellular.apn", "platform.apn", "cellular.auth", "chap", "cellular.username", "").Err(); err != nil {
		t.Fatal(err)
	}
	sub := r.Subscribe(ctx, "settings")
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	c := models.CellularConfig{APN: "requested.apn", Username: "user", Password: "secret", Auth: "pap"}
	if err := SeedSettings(ctx, r, c); err != nil {
		t.Fatal(err)
	}
	values, err := r.HGetAll(ctx, "settings").Result()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"cellular.apn": "platform.apn", "cellular.auth": "chap", "cellular.username": "user", "cellular.password": "secret"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("got %v", values)
	}
	for _, field := range []string{"cellular.username", "cellular.password"} {
		msg, err := sub.ReceiveMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if msg.Payload != field {
			t.Fatalf("notification=%q want %q", msg.Payload, field)
		}
	}
	if err := SeedSettings(ctx, r, models.CellularConfig{APN: "other", Password: "replacement"}); err != nil {
		t.Fatal(err)
	}
	if r.HGet(ctx, "settings", "cellular.password").Val() != "secret" {
		t.Fatal("overwrote password")
	}
}

func TestSeedSettingsDefaults(t *testing.T) {
	m := miniredis.RunT(t)
	r := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer r.Close()
	if err := SeedSettings(context.Background(), r, models.CellularConfig{APN: "internet"}); err != nil {
		t.Fatal(err)
	}
	if got := r.HGet(context.Background(), "settings", "cellular.auth").Val(); got != "none" {
		t.Fatalf("auth=%q", got)
	}
}

func TestApplyAttachAndDataBearer(t *testing.T) {
	for _, tc := range []struct{ auth, command string }{
		{"none", "AT+CGAUTH=1,0"},
		{"pap", `AT+CGAUTH=1,1,"user","secret"`},
		{"chap", `AT+CGAUTH=1,2,"user","secret"`},
	} {
		t.Run(tc.auth, func(t *testing.T) {
			var calls [][]string
			run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				if name == "mmcli" && reflect.DeepEqual(args, []string{"-J", "-L"}) {
					return []byte(`{"modem-list":["/org/freedesktop/ModemManager1/Modem/7"]}`), nil
				}
				// The AT fallback must work when MM's generic EPS setter fails.
				if strings.HasPrefix(args[len(args)-1], "--3gpp-set-initial-eps") {
					return nil, errors.New("unsupported")
				}
				return nil, nil
			}
			err := Apply(context.Background(), models.CellularConfig{APN: "internet", Username: "user", Password: "secret", Auth: tc.auth}, run)
			if err != nil {
				t.Fatal(err)
			}
			if calls[1][0] != "nmcli" || calls[1][3] != "wwan" {
				t.Fatal("wrong data connection")
			}
			var at []string
			for _, call := range calls {
				for _, arg := range call {
					if strings.HasPrefix(arg, "--command=") {
						at = append(at, strings.TrimPrefix(arg, "--command="))
					}
				}
			}
			want := []string{`AT+CGDCONT=1,"IP","internet"`, tc.command, "AT+COPS=2", "AT+COPS=0"}
			if !reflect.DeepEqual(at, want) {
				t.Fatalf("AT commands=%v", at)
			}
			last := calls[len(calls)-1]
			if !reflect.DeepEqual(last, []string{"nmcli", "connection", "up", "wwan"}) {
				t.Fatalf("last call=%v", last)
			}
		})
	}
}

func TestApplyClearAndCustomConnection(t *testing.T) {
	var calls [][]string
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(`{"modem-list":["/modem/3"]}`), nil
	}
	if err := Apply(context.Background(), models.CellularConfig{Connection: "mobile"}, run); err != nil {
		t.Fatal(err)
	}
	if calls[1][3] != "mobile" {
		t.Fatal("custom connection ignored")
	}
	found := false
	for _, call := range calls {
		for _, arg := range call {
			if arg == `--command=AT+CGDCONT=1,"IPV4V6",""` {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("missing default attach context")
	}
}

func TestApplyRejectsBeforeExecuting(t *testing.T) {
	err := Apply(context.Background(), models.CellularConfig{Password: "unsafe\n"}, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("executed invalid config")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestATResponseErrors(t *testing.T) {
	for _, response := range []string{"response: 'ERROR'", "response: '+CME ERROR: 3'", "\r\n+CMS ERROR: 42\r\n"} {
		err := sendAT(context.Background(), func(context.Context, string, ...string) ([]byte, error) {
			return []byte(response), nil
		}, "/modem/3", "AT+CGAUTH=1,0")
		if err == nil {
			t.Fatalf("accepted modem error response %q", response)
		}
	}
}

func TestApplyAuthenticationFailure(t *testing.T) {
	err := Apply(context.Background(), models.CellularConfig{APN: "internet", Auth: "pap"}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if strings.HasPrefix(arg, "--command=AT+CGAUTH") {
				return nil, errors.New("failed")
			}
		}
		return []byte(`{"modem-list":["/modem/3"]}`), nil
	})
	if err == nil {
		t.Fatal("authentication failure reported as success")
	}
}
