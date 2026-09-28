package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"radio-gaga/internal/models"
)

func TestCommandConfigTypeErrors(t *testing.T) {
	tests := []struct {
		name    string
		command string
		params  map[string]interface{}
		want    string
	}{
		{"honk duration", "honk", map[string]interface{}{"on_time": 100}, "honk on_time"},
		{"locate duration", "locate", map[string]interface{}{"honk_interval": 80}, "locate honk_interval"},
		{"alarm boolean", "alarm", map[string]interface{}{"hazards.flash": "yes"}, "alarm hazards.flash"},
		{"alarm duration", "alarm", map[string]interface{}{"horn.on_time": 400}, "alarm horn.on_time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &models.Config{Commands: map[string]models.Command{
				tt.command: {Params: tt.params},
			}}
			client := &ClientImplementation{Config: cfg}
			var err error
			switch tt.command {
			case "honk":
				err = handleHonkCommand(client, nil, context.Background())
			case "locate":
				err = handleLocateCommand(client, nil, context.Background())
			case "alarm":
				err = handleAlarmCommand(client, nil, context.Background(), map[string]interface{}{"duration": "1s"})
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAlarmRejectsInvalidHornCycle(t *testing.T) {
	for _, tt := range []struct {
		on, off string
	}{
		{"0s", "0s"},
		{"bad", "400ms"},
		{"-1s", "400ms"},
		{"2562047h", "2562047h"},
	} {
		if err := startAlarmWithConfig(nil, context.Background(), time.Second, true, true, tt.on, tt.off); err == nil {
			t.Fatalf("horn cycle %q/%q accepted", tt.on, tt.off)
		}
	}
}

func TestRedisCommandRejectsNonStringArguments(t *testing.T) {
	for _, tt := range []struct {
		cmd  string
		args []interface{}
	}{
		{"get", []interface{}{123}},
		{"set", []interface{}{nil, "value"}},
		{"hget", []interface{}{"key", 2}},
		{"hset", []interface{}{"key", nil, "value"}},
		{"lpush", []interface{}{false, "value"}},
	} {
		if err := handleRedisCommand(nil, nil, context.Background(), nil, nil, map[string]interface{}{"cmd": tt.cmd, "args": tt.args}, ""); err == nil {
			t.Fatalf("redis %s accepted invalid args %v", tt.cmd, tt.args)
		}
	}
}
