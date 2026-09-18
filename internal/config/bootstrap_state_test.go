package config

import (
	"testing"

	"radio-gaga/internal/models"
)

// The bootstrap stanza has to be loadable with no scooter token, because the
// token does not exist until the claim is accepted — and that is the whole point
// of the mode. Before this, ValidateConfig rejected such a config outright.
func TestValidateConfigAcceptsBootstrapState(t *testing.T) {
	cfg := &models.Config{
		Bootstrap: models.BootstrapConfig{Code: "FGHX7A", APIBaseURL: "https://sunshine.rescoot.org"},
		RedisURL:  "redis://127.0.0.1:6379",
		MQTT:      models.MQTTConfig{BrokerURL: "ssl://mqtt2.sunshine.rescoot.org:8883", KeepAlive: "30s"},
	}

	if !cfg.InBootstrapState() {
		t.Fatal("a config with a code and no token is bootstrap state")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("bootstrap state must validate, got: %v", err)
	}
}

// A config that merely lost its token must still fail loudly. Silent fallback to
// announce mode would turn a broken install into a scooter that quietly talks to
// the cloud as nobody.
func TestValidateConfigStillRequiresATokenWithoutTheStanza(t *testing.T) {
	cfg := &models.Config{
		Scooter:  models.ScooterConfig{Identifier: "WUNU2S4BXLZ000348"},
		RedisURL: "redis://127.0.0.1:6379",
		MQTT:     models.MQTTConfig{BrokerURL: "ssl://mqtt2.sunshine.rescoot.org:8883", KeepAlive: "30s"},
	}

	if cfg.InBootstrapState() {
		t.Fatal("no stanza means not bootstrap state")
	}
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("a missing token without the stanza must still be rejected")
	}
}

// A scooter that has a real token is done being bootstrapped, even if the stanza
// is left on disk: the identity wins.
func TestTokenMeansNotBootstrapState(t *testing.T) {
	cfg := &models.Config{
		Bootstrap: models.BootstrapConfig{Code: "FGHX7A"},
		Scooter:   models.ScooterConfig{Identifier: "WUNU2S4BXLZ000348", Token: "secret"},
	}

	if cfg.InBootstrapState() {
		t.Fatal("a config with a token is not in bootstrap state")
	}
}
