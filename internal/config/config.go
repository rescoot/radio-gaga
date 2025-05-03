package config

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"radio-gaga/internal/models"

	"gopkg.in/yaml.v2"
)

// ParseFlags parses command line flags and returns them as a struct
func ParseFlags() *models.CommandLineFlags {
	flags := &models.CommandLineFlags{}

	// Basic configuration
	flag.StringVar(&flags.ConfigPath, "config", "", "path to config file (defaults to radio-gaga.yml if not specified)")
	flag.StringVar(&flags.Environment, "environment", "", "environment (production or development)")
	flag.StringVar(&flags.Identifier, "identifier", "", "vehicle identifier (MQTT username)")
	flag.StringVar(&flags.Token, "token", "", "authentication token (MQTT password)")
	flag.StringVar(&flags.MqttBrokerURL, "mqtt-broker", "", "MQTT broker URL")
	flag.StringVar(&flags.MqttCACert, "mqtt-cacert", "", "path to MQTT CA certificate")
	flag.StringVar(&flags.MqttKeepAlive, "mqtt-keepalive", "30s", "MQTT keepalive duration")
	flag.StringVar(&flags.RedisURL, "redis-url", "redis://localhost:6379", "Redis URL")
	flag.BoolVar(&flags.Debug, "debug", false, "enable debug logging")

	// Telemetry intervals
	flag.StringVar(&flags.DrivingInterval, "driving-interval", "1s", "telemetry interval while driving")
	flag.StringVar(&flags.StandbyInterval, "standby-interval", "5m", "telemetry interval in standby")
	flag.StringVar(&flags.StandbyNoBatteryInterval, "standby-no-battery-interval", "8h", "telemetry interval in standby without battery")
	flag.StringVar(&flags.HibernateInterval, "hibernate-interval", "24h", "telemetry interval in hibernate mode")

	flag.Parse()
	return flags
}

// LoadConfig loads configuration from file and/or command line flags
func LoadConfig(flags *models.CommandLineFlags) (*models.Config, error) {
	var config *models.Config

	// Try to load config file
	configPath := flags.ConfigPath
	if configPath == "" {
		configPath = "radio-gaga.yml"
	}

	// Try to read the config file
	if data, err := os.ReadFile(configPath); err == nil {
		config = &models.Config{}
		if err := yaml.Unmarshal(data, config); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %v", err)
		}
		fmt.Printf("Loaded configuration from %s\n", configPath)
	} else if flags.ConfigPath != "" {
		// Only return error if config file was explicitly specified
		return nil, fmt.Errorf("failed to read config file: %v", err)
	} else {
		// Initialize with default values
		config = &models.Config{
			Scooter:     models.ScooterConfig{},
			Environment: "production",
			MQTT: models.MQTTConfig{
				KeepAlive: "180s",
			},
			RedisURL: "redis://127.0.0.1:6379",
			Telemetry: models.TelemetryConfig{
				Intervals: models.TelemetryIntervals{
					Driving:          "1m",
					Standby:          "5m",
					StandbyNoBattery: "8h",
					Hibernate:        "24h",
				},
			},
		}
	}

	// Override with command line flags
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "identifier":
			config.Scooter.Identifier = flags.Identifier
		case "token":
			config.Scooter.Token = flags.Token
		case "environment":
			config.Environment = flags.Environment
		case "mqtt-broker":
			config.MQTT.BrokerURL = flags.MqttBrokerURL
		case "mqtt-cacert":
			config.MQTT.CACert = flags.MqttCACert
		case "mqtt-keepalive":
			config.MQTT.KeepAlive = flags.MqttKeepAlive
		case "redis-url":
			config.RedisURL = flags.RedisURL
		case "driving-interval":
			config.Telemetry.Intervals.Driving = flags.DrivingInterval
		case "standby-interval":
			config.Telemetry.Intervals.Standby = flags.StandbyInterval
		case "standby-no-battery-interval":
			config.Telemetry.Intervals.StandbyNoBattery = flags.StandbyNoBatteryInterval
		case "hibernate-interval":
			config.Telemetry.Intervals.Hibernate = flags.HibernateInterval
		case "debug":
			config.Debug = flags.Debug
		}
	})

	// Validate the final configuration
	if err := ValidateConfig(config); err != nil {
		return nil, fmt.Errorf("invalid configuration: %v", err)
	}

	// Set service name if not specified
	if config.ServiceName == "" {
		config.ServiceName = DetectServiceName()
		fmt.Printf("Auto-detected systemd service name: %s\n", config.ServiceName)
	}

	return config, nil
}

// ValidateConfig validates configuration
func ValidateConfig(config *models.Config) error {
	var errors []string

	if config.Scooter.Identifier == "" {
		errors = append(errors, "scooter identifier is required")
	}
	if config.Scooter.Token == "" {
		errors = append(errors, "scooter token is required")
	}
	if config.Environment != "" && config.Environment != "production" && config.Environment != "development" {
		errors = append(errors, fmt.Sprintf("invalid environment: %s (must be 'production' or 'development')", config.Environment))
	}
	if config.RedisURL == "" {
		errors = append(errors, "redis URL is required")
	}

	// Validate telemetry intervals
	if config.Telemetry.Intervals.Driving == "" {
		config.Telemetry.Intervals.Driving = "1s"
	}
	if config.Telemetry.Intervals.Standby == "" {
		config.Telemetry.Intervals.Standby = "5m"
	}
	if config.Telemetry.Intervals.StandbyNoBattery == "" {
		config.Telemetry.Intervals.StandbyNoBattery = "8h"
	}
	if config.Telemetry.Intervals.Hibernate == "" {
		config.Telemetry.Intervals.Hibernate = "24h"
	}

	// Parse and validate durations
	durations := map[string]string{
		"mqtt.keep_alive":    config.MQTT.KeepAlive,
		"driving":            config.Telemetry.Intervals.Driving,
		"standby":            config.Telemetry.Intervals.Standby,
		"standby_no_battery": config.Telemetry.Intervals.StandbyNoBattery,
		"hibernate":          config.Telemetry.Intervals.Hibernate,
	}
	for name, value := range durations {
		if _, err := time.ParseDuration(value); err != nil {
			errors = append(errors, fmt.Sprintf("invalid %s: %v", name, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration validation failed:\n- %s", strings.Join(errors, "\n- "))
	}

	return nil
}

// DetectServiceName tries to determine the systemd service name from the current process
func DetectServiceName() string {
	// Method 1: Check through cgroups (modern systemd)
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			if strings.Contains(line, ".service") {
				parts := strings.Split(line, ".service")
				if len(parts) > 0 {
					serviceParts := strings.Split(parts[0], "/")
					if len(serviceParts) > 0 {
						serviceName := serviceParts[len(serviceParts)-1] + ".service"
						if serviceName != "" && serviceName != "-.service" {
							fmt.Printf("Detected service name from cgroup: %s\n", serviceName)
							return serviceName
						}
					}
				}
			}
		}
	}
	
	// Method 2: Try using systemd-detect-virt command
	cmd := exec.Command("systemctl", "status", fmt.Sprintf("%d", os.Getpid()))
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, ".service") {
				for _, part := range strings.Fields(line) {
					if strings.HasSuffix(part, ".service") {
						fmt.Printf("Detected service name from systemctl status: %s\n", part)
						return part
					}
				}
			}
		}
	}

	// Method 3: Check environment variable if set
	if serviceName := os.Getenv("SYSTEMD_SERVICE_NAME"); serviceName != "" {
		fmt.Printf("Using service name from environment: %s\n", serviceName)
		return serviceName
	}

	// Default to rescoot-radio-gaga.service if we can't detect it
	fmt.Printf("Could not detect service name, using default: rescoot-radio-gaga.service\n")
	return "rescoot-radio-gaga.service"
}
