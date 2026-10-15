package config

import (
	"testing"

	"radio-gaga/internal/models"

	"gopkg.in/yaml.v2"
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

func TestBootstrapInstallationIDPersistsInYAML(t *testing.T) {
	const id = "797ad894-3fb7-41bc-af68-e534a92b2546"
	var cfg models.Config
	if err := yaml.Unmarshal([]byte("bootstrap:\n  code: FGHX7A\n  installation_id: "+id+"\nscooter:\n  name: My scooter\nredis_url: redis://127.0.0.1:6379\nmqtt:\n  keepalive: 30s\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	body, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored models.Config
	if err := yaml.Unmarshal(body, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Bootstrap.InstallationID != id || restored.Scooter.Name != "My scooter" || !restored.InBootstrapState() {
		t.Fatal("bootstrap identity must survive saving and reloading the config")
	}
}

func TestBootstrapInstallationIDValidation(t *testing.T) {
	for _, id := range []string{
		"../config", "+", "#", "id/other", "not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"797AD894-3FB7-41BC-AF68-E534A92B2546", "797ad894-3fb7-41bc-af68-e534a92b2546\n",
	} {
		cfg := &models.Config{
			Bootstrap: models.BootstrapConfig{Code: "FGHX7A", InstallationID: id},
			RedisURL:  "redis://127.0.0.1:6379",
			MQTT:      models.MQTTConfig{KeepAlive: "30s"},
		}
		if err := ValidateConfig(cfg); err == nil {
			t.Errorf("accepted unsafe installation ID %q", id)
		}
	}
}

// A config that merely lost its token must still fail loudly. Silent fallback to
// announce mode would turn a broken install into a scooter that quietly talks to
// the cloud as nobody.
func TestValidateConfigStillRequiresATokenWithoutTheStanza(t *testing.T) {
	cfg := &models.Config{
		Scooter:  models.ScooterConfig{Identifier: "WUNU2S4BXLZ000000"},
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
		Scooter:   models.ScooterConfig{Identifier: "WUNU2S4BXLZ000000", Token: "secret"},
	}

	if cfg.InBootstrapState() {
		t.Fatal("a config with a token is not in bootstrap state")
	}
}
