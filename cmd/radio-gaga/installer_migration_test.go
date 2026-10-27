package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestPrepareInstallerConfig(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.yaml")
	dest := filepath.Join(dir, "staged.yaml")
	original := "scooter: {identifier: test, token: synthetic-secret}\nmqtt: {broker_url: 'ssl://unreachable.invalid:8883', username: 'example'}\n"
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareInstallerConfig(source, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[interface{}]interface{}
	if err := yaml.UnmarshalStrict([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, &after); err != nil {
		t.Fatal(err)
	}
	beforeMQTT := before["mqtt"].(map[interface{}]interface{})
	afterMQTT := after["mqtt"].(map[interface{}]interface{})
	if afterMQTT["require_verified_tls"] != true {
		t.Fatal("verified TLS was not enabled")
	}
	delete(afterMQTT, "require_verified_tls")
	beforeBytes, _ := yaml.Marshal(before)
	afterBytes, _ := yaml.Marshal(after)
	if string(beforeBytes) != string(afterBytes) || beforeMQTT["username"] != "example" {
		t.Fatal("existing settings changed")
	}
	if bytes, _ := os.ReadFile(source); string(bytes) != original {
		t.Fatal("source changed")
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0600 {
		t.Fatal("staging permissions")
	}
	if err := prepareInstallerConfig(source, dest); err == nil {
		t.Fatal("overwrote staged file")
	}
}

func TestPrepareInstallerConfigRejectsExplicitPolicyAndInvalidTLS(t *testing.T) {
	for _, mqtt := range []string{
		"broker_url: 'ssl://host:8883', require_verified_tls: false",
		"broker_url: 'ssl://host:8883', require_verified_tls: true",
		"broker_url: 'tcp://host:1883'",
		"broker_url: 'ssl://host:8883', require_verified_tls: false, require_verified_tls: true",
	} {
		dir := t.TempDir()
		source := filepath.Join(dir, "source")
		dest := filepath.Join(dir, "dest")
		os.WriteFile(source, []byte("scooter: {identifier: test, token: secret}\nmqtt: {"+mqtt+"}\n"), 0600)
		err := prepareInstallerConfig(source, dest)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unexpected error %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("invalid output retained")
		}
	}
}
