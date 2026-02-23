package notifications

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/models"
)

// RuleMatch represents a triggered rule evaluation result
type RuleMatch struct {
	RuleName   string
	CondValues map[string]string
	Channels   []string
	Message    string // rendered custom message, empty if no template
}

// RuleEvaluator evaluates configurable notification rules against Redis state changes
type RuleEvaluator struct {
	rules       []parsedRule
	redisClient *redis.Client

	mu        sync.Mutex
	cooldowns map[string]time.Time // rule name → last fired time
}

type parsedRule struct {
	models.NotificationRule
	cooldownDur time.Duration
}

// NewRuleEvaluator creates a RuleEvaluator from the given rules config and Redis client
func NewRuleEvaluator(rules []models.NotificationRule, redisClient *redis.Client) *RuleEvaluator {
	parsed := make([]parsedRule, 0, len(rules))
	for _, r := range rules {
		pr := parsedRule{NotificationRule: r}
		if r.Cooldown != "" {
			d, err := time.ParseDuration(r.Cooldown)
			if err != nil {
				log.Printf("[RuleEvaluator] Invalid cooldown %q for rule %q: %v", r.Cooldown, r.Name, err)
			} else {
				pr.cooldownDur = d
			}
		}
		parsed = append(parsed, pr)
	}
	return &RuleEvaluator{
		rules:       parsed,
		redisClient: redisClient,
		cooldowns:   make(map[string]time.Time),
	}
}

// Sources returns a deduplicated list of all hash/channel sources referenced by rules
func (re *RuleEvaluator) Sources() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, r := range re.rules {
		for _, c := range r.Conditions {
			if _, ok := seen[c.Source]; !ok {
				seen[c.Source] = struct{}{}
				out = append(out, c.Source)
			}
		}
	}
	return out
}

// EvaluateHash evaluates rules triggered by a hash change.
// changedHash is the Redis hash key; fields is its full HGetAll result.
func (re *RuleEvaluator) EvaluateHash(ctx context.Context, changedHash string, fields map[string]string) []RuleMatch {
	var result []RuleMatch

	for _, rule := range re.rules {
		if !re.ruleHasHashCondition(rule, changedHash) {
			continue
		}
		if !re.cooldownOK(rule) {
			continue
		}
		condValues, ok := re.evaluateAllConditions(ctx, rule, changedHash, fields)
		if !ok {
			continue
		}
		re.recordFired(rule)
		result = append(result, re.buildMatch(rule, condValues))
	}

	return result
}

// EvaluateMessage evaluates rules triggered by a raw pub/sub message.
func (re *RuleEvaluator) EvaluateMessage(ctx context.Context, changedChannel string, message string) []RuleMatch {
	var result []RuleMatch

	for _, rule := range re.rules {
		if !re.ruleHasMessageCondition(rule, changedChannel) {
			continue
		}
		if !re.cooldownOK(rule) {
			continue
		}
		condValues, ok := re.evaluateAllConditionsForMessage(ctx, rule, changedChannel, message)
		if !ok {
			continue
		}
		re.recordFired(rule)
		result = append(result, re.buildMatch(rule, condValues))
	}

	return result
}

// ruleHasHashCondition returns true if any condition in the rule targets changedHash with a field check
func (re *RuleEvaluator) ruleHasHashCondition(rule parsedRule, changedHash string) bool {
	for _, c := range rule.Conditions {
		if c.Source == changedHash && c.Field != "" {
			return true
		}
	}
	return false
}

// ruleHasMessageCondition returns true if any condition targets changedChannel with a message match
func (re *RuleEvaluator) ruleHasMessageCondition(rule parsedRule, changedChannel string) bool {
	for _, c := range rule.Conditions {
		if c.Source == changedChannel && c.Message != "" {
			return true
		}
	}
	return false
}

// cooldownOK returns true if the rule's cooldown has passed (or has no cooldown)
func (re *RuleEvaluator) cooldownOK(rule parsedRule) bool {
	if rule.cooldownDur == 0 {
		return true
	}
	re.mu.Lock()
	last := re.cooldowns[rule.Name]
	re.mu.Unlock()
	return time.Since(last) >= rule.cooldownDur
}

// recordFired updates the cooldown timestamp for the rule
func (re *RuleEvaluator) recordFired(rule parsedRule) {
	if rule.cooldownDur > 0 {
		re.mu.Lock()
		re.cooldowns[rule.Name] = time.Now()
		re.mu.Unlock()
	}
}

// evaluateAllConditions checks all conditions for a hash-triggered rule.
// Returns the map of matched condition values for the event data, and whether all conditions matched.
func (re *RuleEvaluator) evaluateAllConditions(ctx context.Context, rule parsedRule, changedHash string, fields map[string]string) (map[string]string, bool) {
	condValues := make(map[string]string)

	for _, c := range rule.Conditions {
		if c.Field != "" {
			// Hash field condition
			var val string
			if c.Source == changedHash {
				val = fields[c.Field]
			} else {
				// Read from Redis on demand
				v, err := re.redisClient.HGet(ctx, c.Source, c.Field).Result()
				if err != nil {
					if err != redis.Nil {
						log.Printf("[RuleEvaluator] Failed to read %s[%s]: %v", c.Source, c.Field, err)
					}
					return nil, false
				}
				val = v
			}
			if !checkCondition(val, c.Operator, c.Value) {
				return nil, false
			}
			condValues[c.Source+"."+c.Field] = val
		} else if c.Message != "" {
			// Message match — cannot check inline during a hash change (no message available),
			// so this condition always fails when triggered from a hash change
			return nil, false
		}
	}

	return condValues, true
}

// evaluateAllConditionsForMessage checks all conditions for a message-triggered rule.
func (re *RuleEvaluator) evaluateAllConditionsForMessage(ctx context.Context, rule parsedRule, changedChannel string, message string) (map[string]string, bool) {
	condValues := make(map[string]string)

	for _, c := range rule.Conditions {
		if c.Message != "" {
			// Raw message condition
			if c.Source != changedChannel {
				// Different channel — can't evaluate without a message; skip
				return nil, false
			}
			if !checkMessageCondition(message, c.Message, c.Value) {
				return nil, false
			}
			condValues[c.Source+".message"] = message
		} else if c.Field != "" {
			// Hash field condition — read from Redis
			val, err := re.redisClient.HGet(ctx, c.Source, c.Field).Result()
			if err != nil {
				if err != redis.Nil {
					log.Printf("[RuleEvaluator] Failed to read %s[%s]: %v", c.Source, c.Field, err)
				}
				return nil, false
			}
			if !checkCondition(val, c.Operator, c.Value) {
				return nil, false
			}
			condValues[c.Source+"."+c.Field] = val
		}
	}

	return condValues, true
}

// buildMatch constructs a RuleMatch for a triggered rule
func (re *RuleEvaluator) buildMatch(rule parsedRule, condValues map[string]string) RuleMatch {
	m := RuleMatch{
		RuleName:   rule.Name,
		CondValues: condValues,
		Channels:   rule.Channels,
	}
	if rule.Message != "" {
		m.Message = renderMessage(rule.Message, condValues)
	}
	return m
}

// renderMessage replaces {{key}} placeholders with values from condValues.
// Keys match the "source.field" format, e.g. {{cb-battery.charge}}.
func renderMessage(tmpl string, values map[string]string) string {
	result := tmpl
	for k, v := range values {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result
}

// checkCondition evaluates a numeric-or-string comparison.
// Operators: <, >, <=, >=, ==, !=
func checkCondition(value, operator, threshold string) bool {
	// Try numeric comparison first
	fVal, errV := strconv.ParseFloat(value, 64)
	fThr, errT := strconv.ParseFloat(threshold, 64)

	if errV == nil && errT == nil {
		switch operator {
		case "<":
			return fVal < fThr
		case ">":
			return fVal > fThr
		case "<=":
			return fVal <= fThr
		case ">=":
			return fVal >= fThr
		case "==":
			return fVal == fThr
		case "!=":
			return fVal != fThr
		}
	}

	// Fall back to string comparison for == and !=
	switch operator {
	case "==":
		return value == threshold
	case "!=":
		return value != threshold
	}

	return false
}

// checkMessageCondition evaluates a raw pub/sub message match.
// operator: "==" for exact match, "contains" for substring
func checkMessageCondition(message, operator, value string) bool {
	switch operator {
	case "==":
		return message == value
	case "contains":
		return strings.Contains(message, value)
	}
	return false
}
