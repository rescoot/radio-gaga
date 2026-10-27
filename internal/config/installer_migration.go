package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v2"
)

// PrepareInstallerConfig returns a staged copy with verified TLS enabled only
// when the existing setting is absent. Explicit policies are never overridden.
func PrepareInstallerConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document yaml.MapSlice
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		return nil, fmt.Errorf("invalid YAML")
	}
	for i := range document {
		if document[i].Key != "mqtt" {
			continue
		}
		mqtt, ok := document[i].Value.(yaml.MapSlice)
		if !ok {
			return nil, fmt.Errorf("MQTT configuration is not a mapping")
		}
		for _, item := range mqtt {
			if item.Key == "require_verified_tls" {
				return nil, fmt.Errorf("existing TLS policy requires review")
			}
		}
		document[i].Value = append(mqtt, yaml.MapItem{Key: "require_verified_tls", Value: true})
		return yaml.Marshal(document)
	}
	return nil, fmt.Errorf("MQTT configuration is missing")
}
