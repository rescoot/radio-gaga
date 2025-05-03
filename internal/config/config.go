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

	// NTP configuration
	flag.BoolVar(&flags.NtpEnabled, "ntp-enabled", true, "enable NTP time synchronization")
	flag.StringVar(&flags.NtpServer, "ntp-server", "pool.ntp.rescoot.org", "NTP server address")

	// Telemetry intervals
	flag.StringVar(&flags.DrivingInterval, "driving-interval", "1s", "telemetry interval while driving")
	flag.StringVar(&flags.StandbyInterval, "standby-interval", "5m", "telemetry interval in standby")
	flag.StringVar(&flags.StandbyNoBatteryInterval, "standby-no-battery-interval", "8h", "telemetry interval in standby without battery")
	flag.StringVar(&flags.HibernateInterval, "hibernate-interval", "24h", "telemetry interval in hibernate mode")

	// Telemetry buffer options
	flag.BoolVar(&flags.BufferEnabled, "buffer-enabled", false, "enable telemetry buffering")
	flag.IntVar(&flags.BufferMaxSize, "buffer-max-size", 1000, "maximum number of telemetry events to buffer")
	flag.IntVar(&flags.BufferMaxRetries, "buffer-max-retries", 5, "maximum number of retries for sending buffered telemetry")
	flag.StringVar(&flags.BufferRetryInterval, "buffer-retry-interval", "1m", "interval between retries for sending buffered telemetry")
	flag.StringVar(&flags.BufferPersistPath, "buffer-persist-path", "", "path to persist telemetry buffer (empty for no persistence)")
	flag.StringVar(&flags.TransmitPeriod, "transmit-period", "5m", "period for transmitting buffered telemetry")

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
			NTP: models.NTPConfig{
				Enabled: true,
				Server:  "pool.ntp.rescoot.org",
			},
			RedisURL: "redis://127.0.0.1:6379",
			Telemetry: models.TelemetryConfig{
				Intervals: models.TelemetryIntervals{
					Driving:          "1m",
					Standby:          "5m",
					StandbyNoBattery: "8h",
					Hibernate:        "24h",
				},
				Buffer: models.BufferConfig{
					Enabled:       false,
					MaxSize:       1000,
					MaxRetries:    5,
					RetryInterval: "1m",
				},
				TransmitPeriod: "5m",
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
		case "buffer-enabled":
			if config.Telemetry.Buffer.Enabled != flags.BufferEnabled {
				config.Telemetry.Buffer.Enabled = flags.BufferEnabled
			}
		case "buffer-max-size":
			if config.Telemetry.Buffer.MaxSize != flags.BufferMaxSize {
				config.Telemetry.Buffer.MaxSize = flags.BufferMaxSize
			}
		case "buffer-max-retries":
			if config.Telemetry.Buffer.MaxRetries != flags.BufferMaxRetries {
				config.Telemetry.Buffer.MaxRetries = flags.BufferMaxRetries
			}
		case "buffer-retry-interval":
			config.Telemetry.Buffer.RetryInterval = flags.BufferRetryInterval
		case "buffer-persist-path":
			config.Telemetry.Buffer.PersistPath = flags.BufferPersistPath
		case "transmit-period":
			config.Telemetry.TransmitPeriod = flags.TransmitPeriod
		case "debug":
			config.Debug = flags.Debug
		case "ntp-enabled":
			config.NTP.Enabled = flags.NtpEnabled
		case "ntp-server":
			config.NTP.Server = flags.NtpServer
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

	// Initialize buffer config if not present
	if config.Telemetry.Buffer.RetryInterval == "" {
		config.Telemetry.Buffer.RetryInterval = "1m"
	}
	if config.Telemetry.Buffer.MaxSize <= 0 {
		config.Telemetry.Buffer.MaxSize = 1000
	}
	if config.Telemetry.Buffer.MaxRetries <= 0 {
		config.Telemetry.Buffer.MaxRetries = 5
	}

	// Initialize transmit period if not present
	if config.Telemetry.TransmitPeriod == "" {
		config.Telemetry.TransmitPeriod = "5m"
	}

	// Parse and validate durations
	durations := map[string]string{
		"mqtt.keep_alive":        config.MQTT.KeepAlive,
		"driving":                config.Telemetry.Intervals.Driving,
		"standby":                config.Telemetry.Intervals.Standby,
		"standby_no_battery":     config.Telemetry.Intervals.StandbyNoBattery,
		"hibernate":              config.Telemetry.Intervals.Hibernate,
		"buffer.retry_interval":  config.Telemetry.Buffer.RetryInterval,
		"telemetry.transmit_period": config.Telemetry.TransmitPeriod,
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
