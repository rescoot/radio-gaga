package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestConnectSupportedPlatformVersions(t *testing.T) {
	for _, version := range []string{"v1.12.0", "v1.12.7", "v1.13.0", "v1.14.0", "v2.0.0"} {
		t.Run("stock-"+version, func(t *testing.T) {
			platform, err := parseConnectPlatform([]byte(fmt.Sprintf("ID=scooteros-mdb\nVERSION_ID=%s\n", version)))
			if err != nil || platform.Name != "stock" || platform.ConfigPath != "/etc/rescoot/radio-gaga.yml" {
				t.Fatalf("stock platform detection failed: %+v %v", platform, err)
			}
		})
	}
	for _, version := range []string{"1.0.0", "v1.0.1", "1.0.5", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.4.0+build", "2.0.0"} {
		for _, id := range []string{"librescoot-mdb", "librescoot"} {
			t.Run(id+"-"+version, func(t *testing.T) {
				platform, err := parseConnectPlatform([]byte(fmt.Sprintf("ID=\"%s\"\nVERSION_ID=\"%s\"\n", id, version)))
				if err != nil || platform.Name != "librescoot" || platform.ConfigPath != "/data/radio-gaga/config.yaml" {
					t.Fatalf("Librescoot platform detection failed: %+v %v", platform, err)
				}
			})
		}
	}
	for _, data := range []string{"ID=scooteros-mdb\nVERSION_ID=v1.11.9", "ID=librescoot-mdb\nVERSION_ID=0.9.9", "ID=librescoot-dbc\nVERSION_ID=1.4.0", "ID=ubuntu\nVERSION_ID=24.04", "ID=librescoot-mdb"} {
		if _, err := parseConnectPlatform([]byte(data)); err == nil {
			t.Fatalf("unsupported MDB platform accepted: %s", data)
		}
	}
}

func TestConnectServiceNamesAndConfigurationBinding(t *testing.T) {
	for _, expected := range []string{"radio-gaga.service", "rescoot-radio-gaga.service", "librescoot-radio-gaga.service"} {
		actual, err := selectConnectService(func(name string) (string, error) {
			if name == expected {
				return "loaded\n", nil
			}
			return "not-found", nil
		})
		if err != nil || actual != expected {
			t.Fatalf("service detection failed: %s %v", actual, err)
		}
	}
	if _, err := selectConnectService(func(string) (string, error) { return "", errors.New("unavailable") }); err == nil {
		t.Fatal("missing system service accepted")
	}
	if !connectServiceUsesBinary("{ path=/opt/rescoot-radio-gaga/radio-gaga ; argv[]=/opt/rescoot-radio-gaga/radio-gaga }") || connectServiceUsesBinary("{ path=/tmp/radio-gaga ; argv[]=/tmp/radio-gaga }") {
		t.Fatal("service executable binding failed")
	}
	for _, path := range []string{"/etc/rescoot/radio-gaga.yml", "/data/radio-gaga/config.yaml"} {
		for _, separator := range []string{" ", "="} {
			start := "{ path=/usr/bin/radio-gaga ; argv[]=/usr/bin/radio-gaga -config" + separator + path + " ; ignore_errors=no }"
			if !connectServiceUsesConfig(start, path) {
				t.Fatal("managed config path rejected")
			}
			if connectServiceUsesConfig(start, path+".other") {
				t.Fatal("another config path accepted")
			}
		}
		platform := connectPlatform{ConfigPath: path}
		if actual, err := connectConfigPath("", platform); err != nil || actual != path {
			t.Fatal("managed default failed")
		}
		if _, err := connectConfigPath("/tmp/unmanaged", platform); err == nil {
			t.Fatal("unmanaged config target accepted")
		}
	}
}

func TestConnectUpdateSafetyAcrossFirmwareGenerations(t *testing.T) {
	for _, statuses := range [][]any{{nil, nil, nil}, {"initializing", nil, nil}, {"idle", "idle", "complete"}, {nil, "success", "error"}} {
		if !connectUpdatesIdle(statuses) {
			t.Fatalf("inactive update state blocked: %v", statuses)
		}
	}
	for _, statuses := range [][]any{{"downloading", nil, nil}, {nil, "installing", nil}, {nil, nil, "pending-reboot"}, {nil, "initializing", nil}, {"unknown-state", nil, nil}} {
		if connectUpdatesIdle(statuses) {
			t.Fatalf("active or unknown update state accepted: %v", statuses)
		}
	}
}
