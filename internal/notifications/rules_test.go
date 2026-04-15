package notifications

import (
	"context"
	"testing"
	"time"

	"radio-gaga/internal/models"
)

func TestCheckCondition(t *testing.T) {
	tests := []struct {
		value, op, threshold string
		want                 bool
	}{
		{"45", "<", "50", true},
		{"50", "<", "50", false},
		{"55", "<", "50", false},
		{"80", ">", "79", true},
		{"79", ">=", "80", false},
		{"80", ">=", "80", true},
		{"10", "<=", "10", true},
		{"10", "!=", "10", false},
		{"10", "!=", "11", true},
		{"triggered", "==", "triggered", true},
		{"triggered", "==", "armed", false},
		{"triggered", "!=", "armed", true},
	}

	for _, tt := range tests {
		got := checkCondition(tt.value, tt.op, tt.threshold)
		if got != tt.want {
			t.Errorf("checkCondition(%q, %q, %q) = %v, want %v", tt.value, tt.op, tt.threshold, got, tt.want)
		}
	}
}

func TestCheckMessageCondition(t *testing.T) {
	tests := []struct {
		message, op, value string
		want               bool
	}{
		{"triggered", "==", "triggered", true},
		{"triggered", "==", "armed", false},
		{"hello world", "contains", "world", true},
		{"hello world", "contains", "foo", false},
	}

	for _, tt := range tests {
		got := checkMessageCondition(tt.message, tt.op, tt.value)
		if got != tt.want {
			t.Errorf("checkMessageCondition(%q, %q, %q) = %v, want %v", tt.message, tt.op, tt.value, got, tt.want)
		}
	}
}

func TestRuleEvaluator_NoRedis(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "test_rule",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "50"},
			},
			Channels: []string{"telegram"},
		},
	}

	re := NewRuleEvaluator(rules, nil)

	// Rule should match when cb-battery charge < 50
	matches := re.EvaluateHash(context.Background(), "cb-battery", map[string]string{
		"charge": "45",
	})
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].RuleName != "test_rule" {
		t.Errorf("expected rule name %q, got %q", "test_rule", matches[0].RuleName)
	}
	if matches[0].Channels[0] != "telegram" {
		t.Errorf("expected channel %q, got %q", "telegram", matches[0].Channels[0])
	}

	// Rule should NOT match when charge >= 50
	matches = re.EvaluateHash(context.Background(), "cb-battery", map[string]string{
		"charge": "55",
	})
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}

	// Rule should NOT match for a different hash
	matches = re.EvaluateHash(context.Background(), "vehicle", map[string]string{
		"state": "standby",
	})
	if len(matches) != 0 {
		t.Errorf("expected 0 matches for different hash, got %d", len(matches))
	}
}

func TestRuleEvaluator_MessageCondition(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "alarm_rule",
			Conditions: []models.RuleCondition{
				{Source: "alarm", Message: "==", Value: "triggered"},
			},
			Channels: []string{"telegram", "sms"},
		},
	}

	re := NewRuleEvaluator(rules, nil)

	// Should match exact message
	matches := re.EvaluateMessage(context.Background(), "alarm", "triggered")
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].RuleName != "alarm_rule" {
		t.Errorf("expected rule name %q, got %q", "alarm_rule", matches[0].RuleName)
	}

	// Should NOT match different message
	matches = re.EvaluateMessage(context.Background(), "alarm", "armed")
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d", len(matches))
	}
}

func TestRuleEvaluator_Cooldown(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "cooldown_rule",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "50"},
			},
			Channels: []string{"telegram"},
			Cooldown: "1h",
		},
	}

	re := NewRuleEvaluator(rules, nil)

	// First match should succeed
	fields := map[string]string{"charge": "30"}
	matches := re.EvaluateHash(context.Background(), "cb-battery", fields)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match on first evaluation, got %d", len(matches))
	}

	// Second match within cooldown should fail
	matches = re.EvaluateHash(context.Background(), "cb-battery", fields)
	if len(matches) != 0 {
		t.Errorf("expected 0 matches during cooldown, got %d", len(matches))
	}

	// Manually expire cooldown
	re.mu.Lock()
	re.cooldowns["cooldown_rule"] = time.Now().Add(-2 * time.Hour)
	re.mu.Unlock()

	// Should match again after cooldown
	matches = re.EvaluateHash(context.Background(), "cb-battery", fields)
	if len(matches) != 1 {
		t.Errorf("expected 1 match after cooldown expired, got %d", len(matches))
	}
}

func TestRuleEvaluator_CustomMessage(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "cbb_low",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "50"},
			},
			Channels: []string{"telegram"},
			Message:  "CB battery at {{cb-battery.charge}}%",
		},
	}

	re := NewRuleEvaluator(rules, nil)
	matches := re.EvaluateHash(context.Background(), "cb-battery", map[string]string{
		"charge": "42",
	})
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Message != "CB battery at 42%" {
		t.Errorf("expected rendered message %q, got %q", "CB battery at 42%", matches[0].Message)
	}
}

func TestRuleEvaluator_NoMessageTemplate(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "no_msg",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "50"},
			},
			Channels: []string{"telegram"},
		},
	}

	re := NewRuleEvaluator(rules, nil)
	matches := re.EvaluateHash(context.Background(), "cb-battery", map[string]string{
		"charge": "42",
	})
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Message != "" {
		t.Errorf("expected empty message, got %q", matches[0].Message)
	}
}

func TestRenderMessage(t *testing.T) {
	values := map[string]string{
		"cb-battery.charge": "42",
		"vehicle.state":     "standby",
	}

	tests := []struct {
		tmpl, want string
	}{
		{"Battery at {{cb-battery.charge}}%", "Battery at 42%"},
		{"{{cb-battery.charge}}% while {{vehicle.state}}", "42% while standby"},
		{"No placeholders here", "No placeholders here"},
		{"Unknown {{foo.bar}} stays", "Unknown {{foo.bar}} stays"},
	}

	for _, tt := range tests {
		got := renderMessage(tt.tmpl, values)
		if got != tt.want {
			t.Errorf("renderMessage(%q) = %q, want %q", tt.tmpl, got, tt.want)
		}
	}
}

func TestRuleEvaluator_Sources(t *testing.T) {
	rules := []models.NotificationRule{
		{
			Name: "r1",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "50"},
				{Source: "vehicle", Field: "state", Operator: "==", Value: "standby"},
			},
		},
		{
			Name: "r2",
			Conditions: []models.RuleCondition{
				{Source: "cb-battery", Field: "charge", Operator: "<", Value: "20"},
			},
		},
	}

	re := NewRuleEvaluator(rules, nil)
	sources := re.Sources()

	// Should have cb-battery and vehicle (deduped)
	if len(sources) != 2 {
		t.Errorf("expected 2 sources, got %d: %v", len(sources), sources)
	}
}
