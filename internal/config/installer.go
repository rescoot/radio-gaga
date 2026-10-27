package config

import (
	"fmt"
	"os"

	"radio-gaga/internal/models"

	"gopkg.in/yaml.v2"
)

// ReadInstallerConfig deliberately avoids flag overrides, state-directory creation
// and transaction recovery. A preflight must not mutate an enrolled installation.
func ReadInstallerConfig(path string) (*models.Config, error) {
	if path == "" {
		return nil, fmt.Errorf("an explicit configuration path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("invalid YAML")
	}
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if !cfg.MQTT.RequireVerifiedTLS {
		return nil, fmt.Errorf("mqtt.require_verified_tls must be true")
	}
	return cfg, nil
}
