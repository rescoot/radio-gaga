package utils

import (
	"testing"

	"gopkg.in/yaml.v2"
)

func TestLookupCommandParam_FlatDottedKey(t *testing.T) {
	params := map[string]interface{}{"hazards.flash": true, "horn.on_time": "400ms"}

	if got := LookupCommandParam(params, "hazards.flash", false); got != true {
		t.Errorf("flat dotted key hazards.flash: got %v, want true", got)
	}
	if got := LookupCommandParam(params, "horn.on_time", "x"); got != "400ms" {
		t.Errorf("flat dotted key horn.on_time: got %v, want 400ms", got)
	}
}

func TestLookupCommandParam_NestedFromYAML(t *testing.T) {
	// yaml.v2 decodes nested maps as map[interface{}]interface{}; the traversal
	// must handle that, not just map[string]interface{}.
	var doc struct {
		Params map[string]interface{} `yaml:"params"`
	}
	src := `
params:
  hazards:
    flash: true
  horn:
    on_time: 400ms
    honk: false
`
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := LookupCommandParam(doc.Params, "hazards.flash", false); got != true {
		t.Errorf("nested hazards.flash: got %v (%T), want true", got, got)
	}
	if got := LookupCommandParam(doc.Params, "horn.on_time", "x"); got != "400ms" {
		t.Errorf("nested horn.on_time: got %v, want 400ms", got)
	}
	if got := LookupCommandParam(doc.Params, "horn.honk", true); got != false {
		t.Errorf("nested horn.honk: got %v, want false", got)
	}
}

func TestLookupCommandParam_FlatWinsOverNested(t *testing.T) {
	params := map[string]interface{}{
		"hazards.flash": true,
		"hazards":       map[string]interface{}{"flash": false},
	}
	if got := LookupCommandParam(params, "hazards.flash", nil); got != true {
		t.Errorf("flat key should win over nested: got %v, want true", got)
	}
}

func TestLookupCommandParam_MissingReturnsDefault(t *testing.T) {
	params := map[string]interface{}{
		"horn": map[string]interface{}{"on_time": "400ms"},
	}

	if got := LookupCommandParam(params, "horn.off_time", "200ms"); got != "200ms" {
		t.Errorf("missing nested leaf: got %v, want default 200ms", got)
	}
	if got := LookupCommandParam(params, "missing", 42); got != 42 {
		t.Errorf("missing flat key: got %v, want default 42", got)
	}
	if got := LookupCommandParam(nil, "x", "d"); got != "d" {
		t.Errorf("nil params: got %v, want default d", got)
	}
}
