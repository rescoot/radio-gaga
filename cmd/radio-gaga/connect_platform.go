package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type connectPlatform struct {
	Name       string
	Version    string
	ConfigPath string
	Service    string
}

func parseConnectPlatform(data []byte) (connectPlatform, error) {
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	id := values["ID"]
	platform := connectPlatform{Version: values["VERSION_ID"]}
	minimum := [3]int{1, 0, 0}
	switch {
	case id == "librescoot" || id == "librescoot-mdb":
		platform.Name, platform.ConfigPath = "librescoot", "/data/radio-gaga/config.yaml"
	case id == "scooteros" || id == "scooteros-mdb":
		platform.Name, platform.ConfigPath = "stock", "/etc/rescoot/radio-gaga.yml"
		minimum = [3]int{1, 12, 0}
	default:
		return platform, errors.New("enrollment requires a supported MDB platform")
	}
	parts := regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:$|[+~-])`).FindStringSubmatch(platform.Version)
	if parts == nil {
		return platform, errors.New("platform version is unavailable")
	}
	for i := 0; i < 3; i++ {
		value, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return platform, errors.New("invalid platform version")
		}
		if value > minimum[i] {
			return platform, nil
		}
		if value < minimum[i] {
			return platform, errors.New("enrollment requires scooterOS 1.12.0+ or Librescoot 1.0.0+")
		}
	}
	return platform, nil
}

func selectConnectService(loadState func(string) (string, error)) (string, error) {
	// Both firmware generations and separately installed clients use these names.
	for _, name := range []string{"radio-gaga.service", "rescoot-radio-gaga.service", "librescoot-radio-gaga.service"} {
		state, err := loadState(name)
		if err == nil && strings.TrimSpace(state) == "loaded" {
			return name, nil
		}
	}
	return "", errors.New("install radio-gaga as a system service before enrollment")
}

func detectConnectPlatform(ctx context.Context) (connectPlatform, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return connectPlatform{}, errors.New("platform identification is unavailable")
	}
	platform, err := parseConnectPlatform(data)
	if err != nil {
		return platform, err
	}
	platform.Service, err = selectConnectService(func(name string) (string, error) {
		command := exec.CommandContext(ctx, "systemctl", "show", name, "--property=LoadState", "--value")
		data, err := command.Output()
		return string(data), err
	})
	if err != nil {
		return platform, err
	}
	command := exec.CommandContext(ctx, "systemctl", "show", platform.Service, "--property=ExecStart", "--value")
	start, err := command.Output()
	if err != nil || !connectServiceUsesConfig(string(start), platform.ConfigPath) || !connectServiceUsesBinary(string(start)) {
		return platform, errors.New("radio-gaga service does not use the managed platform configuration")
	}
	return platform, nil
}

func connectServiceUsesBinary(start string) bool {
	for _, path := range []string{"/opt/rescoot-radio-gaga/radio-gaga", "/usr/bin/radio-gaga", "/sbin/radio-gaga"} {
		if strings.Contains(start, "path="+path+" ") || strings.Contains(start, "path="+path+";") {
			return true
		}
	}
	return false
}

func connectServiceUsesConfig(start, path string) bool {
	pattern := `(?:^|\s)--?config(?:=|\s+)` + regexp.QuoteMeta(path) + `(?:\s|;|$)`
	return regexp.MustCompile(pattern).MatchString(start)
}

func connectConfigPath(requested string, platform connectPlatform) (string, error) {
	if requested != "" && requested != platform.ConfigPath {
		return "", fmt.Errorf("managed enrollment requires %s", platform.ConfigPath)
	}
	return platform.ConfigPath, nil
}
