package config

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"reflect"
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
func LoadConfig(flags *models.CommandLineFlags) (*models.Config, string, error) {
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
			return nil, "", fmt.Errorf("failed to parse config file: %v", err)
		}
		log.Printf("Loaded configuration from %s", configPath)
	} else if flags.ConfigPath != "" {
		// Only return error if config file was explicitly specified
		return nil, "", fmt.Errorf("failed to read config file: %v", err)
	} else {
		// Initialize with default values
		config = &models.Config{
			Scooter:     models.ScooterConfig{},
			Environment: "production",
			MQTT: models.MQTTConfig{
				KeepAlive: "30s",
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
		return nil, "", fmt.Errorf("invalid configuration: %v", err)
	}

	// Set service name if not specified
	if config.ServiceName == "" {
		config.ServiceName = DetectServiceName()
		log.Printf("Auto-detected systemd service name: %s", config.ServiceName)
	}

	return config, configPath, nil
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

	// Initialize priority config with defaults
	if config.Telemetry.Priorities.Immediate == "" {
		config.Telemetry.Priorities.Immediate = "1s"
	}
	if config.Telemetry.Priorities.Quick == "" {
		config.Telemetry.Priorities.Quick = "5s"
	}
	if config.Telemetry.Priorities.Medium == "" {
		config.Telemetry.Priorities.Medium = "1m"
	}
	if config.Telemetry.Priorities.Slow == "" {
		config.Telemetry.Priorities.Slow = "15m"
	}

	// Initialize events config with defaults
	if config.Events.MaxRetries <= 0 {
		config.Events.MaxRetries = 10
	}
	if config.Events.BufferPath == "" {
		config.Events.BufferPath = "/var/lib/radio-gaga/events-buffer.json"
	}

	// Parse and validate durations
	durations := map[string]string{
		"mqtt.keep_alive":           config.MQTT.KeepAlive,
		"driving":                   config.Telemetry.Intervals.Driving,
		"standby":                   config.Telemetry.Intervals.Standby,
		"standby_no_battery":        config.Telemetry.Intervals.StandbyNoBattery,
		"hibernate":                 config.Telemetry.Intervals.Hibernate,
		"buffer.retry_interval":     config.Telemetry.Buffer.RetryInterval,
		"telemetry.transmit_period": config.Telemetry.TransmitPeriod,
		"priorities.immediate":      config.Telemetry.Priorities.Immediate,
		"priorities.quick":          config.Telemetry.Priorities.Quick,
		"priorities.medium":         config.Telemetry.Priorities.Medium,
		"priorities.slow":           config.Telemetry.Priorities.Slow,
	}
	for name, value := range durations {
		if _, err := time.ParseDuration(value); err != nil {
			errors = append(errors, fmt.Sprintf("invalid %s: %v", name, err))
		}
	}

	// Validate priority ordering: immediate <= quick <= medium <= slow
	if err := validatePriorityOrdering(config); err != nil {
		errors = append(errors, err.Error())
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
							log.Printf("Detected service name from cgroup: %s", serviceName)
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
						log.Printf("Detected service name from systemctl status: %s", part)
						return part
					}
				}
			}
		}
	}

	// Method 3: Check environment variable if set
	if serviceName := os.Getenv("SYSTEMD_SERVICE_NAME"); serviceName != "" {
		log.Printf("Using service name from environment: %s", serviceName)
		return serviceName
	}

	// Default to rescoot-radio-gaga.service if we can't detect it
	log.Printf("Could not detect service name, using default: rescoot-radio-gaga.service")
	return "rescoot-radio-gaga.service"
}

// SaveConfig saves the configuration to a YAML file
func SaveConfig(config *models.Config, configPath string) error {
	// Marshal the config to YAML
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config to YAML: %v", err)
	}

	// Create a backup of the existing config file if it exists
	if _, err := os.Stat(configPath); err == nil {
		backupPath := configPath + ".backup"
		if err := copyFile(configPath, backupPath); err != nil {
			log.Printf("Warning: failed to create backup of config file: %v", err)
		} else {
			log.Printf("Created backup of config file at %s", backupPath)
		}
	}

	// Write the new config file
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	log.Printf("Configuration saved to %s", configPath)
	return nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// setNestedField sets a nested field in a struct using reflection
func setNestedField(obj interface{}, parts []string, value interface{}) error {
	if len(parts) == 0 {
		return fmt.Errorf("empty field path")
	}

	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("object must be a pointer to a struct")
	}

	v = v.Elem()

	// Navigate to the nested field
	for i, part := range parts[:len(parts)-1] {
		field := v.FieldByName(titleCase(part))
		if !field.IsValid() {
			return fmt.Errorf("field '%s' not found at level %d", part, i)
		}
		if field.Kind() == reflect.Ptr {
			if field.IsNil() {
				return fmt.Errorf("nil pointer encountered at field '%s'", part)
			}
			field = field.Elem()
		}
		if field.Kind() != reflect.Struct {
			return fmt.Errorf("field '%s' is not a struct", part)
		}
		v = field
	}

	// Set the final field
	finalField := titleCase(parts[len(parts)-1])
	field := v.FieldByName(finalField)
	if !field.IsValid() {
		return fmt.Errorf("field '%s' not found", finalField)
	}
	if !field.CanSet() {
		return fmt.Errorf("field '%s' cannot be set", finalField)
	}

	// Convert and set the value
	return setFieldValue(field, value)
}

// setFieldValue sets a field value with type conversion
func setFieldValue(field reflect.Value, value interface{}) error {
	valueReflect := reflect.ValueOf(value)
	fieldType := field.Type()

	// Handle type conversions
	switch fieldType.Kind() {
	case reflect.String:
		if valueReflect.Kind() == reflect.String {
			field.SetString(valueReflect.String())
		} else {
			field.SetString(fmt.Sprintf("%v", value))
		}
	case reflect.Bool:
		if valueReflect.Kind() == reflect.Bool {
			field.SetBool(valueReflect.Bool())
		} else {
			// Try to parse string as bool
			if str, ok := value.(string); ok {
				if str == "true" || str == "1" {
					field.SetBool(true)
				} else if str == "false" || str == "0" {
					field.SetBool(false)
				} else {
					return fmt.Errorf("cannot convert '%s' to bool", str)
				}
			} else {
				return fmt.Errorf("cannot convert %T to bool", value)
			}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if valueReflect.Kind() >= reflect.Int && valueReflect.Kind() <= reflect.Int64 {
			field.SetInt(valueReflect.Int())
		} else if valueReflect.Kind() >= reflect.Uint && valueReflect.Kind() <= reflect.Uint64 {
			field.SetInt(int64(valueReflect.Uint()))
		} else if valueReflect.Kind() >= reflect.Float32 && valueReflect.Kind() <= reflect.Float64 {
			field.SetInt(int64(valueReflect.Float()))
		} else {
			return fmt.Errorf("cannot convert %T to int", value)
		}
	default:
		if valueReflect.Type().ConvertibleTo(fieldType) {
			field.Set(valueReflect.Convert(fieldType))
		} else {
			return fmt.Errorf("cannot convert %T to %s", value, fieldType)
		}
	}

	return nil
}

// titleCase converts a string to title case (first letter uppercase)
func titleCase(s string) string {
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// GetConfigField retrieves a specific field from the configuration using YAML dot syntax
func GetConfigField(config *models.Config, field string) (interface{}, error) {
	structPath := convertYamlPathToStructPath(field)
	parts := strings.Split(structPath, ".")
	return getNestedField(config, parts)
}

// SetConfigField sets a specific field in the configuration using YAML dot syntax
func SetConfigField(config *models.Config, field string, value interface{}) error {
	structPath := convertYamlPathToStructPath(field)
	parts := strings.Split(structPath, ".")
	return setNestedFieldUnrestricted(config, parts, value)
}

// DeleteConfigField clears/resets a specific field in the configuration using YAML dot syntax
func DeleteConfigField(config *models.Config, field string) error {
	structPath := convertYamlPathToStructPath(field)
	parts := strings.Split(structPath, ".")
	return deleteNestedField(config, parts)
}

// getNestedField retrieves a nested field value using reflection
func getNestedField(obj interface{}, parts []string) (interface{}, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty field path")
	}

	v := reflect.ValueOf(obj)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("object must be a struct or pointer to struct")
	}

	// Navigate to the nested field
	for i, part := range parts[:len(parts)-1] {
		field := v.FieldByName(titleCase(part))
		if !field.IsValid() {
			return nil, fmt.Errorf("field '%s' not found at level %d", part, i)
		}
		if field.Kind() == reflect.Ptr {
			if field.IsNil() {
				return nil, fmt.Errorf("nil pointer encountered at field '%s'", part)
			}
			field = field.Elem()
		}
		if field.Kind() != reflect.Struct {
			return nil, fmt.Errorf("field '%s' is not a struct", part)
		}
		v = field
	}

	// Get the final field
	finalField := titleCase(parts[len(parts)-1])
	field := v.FieldByName(finalField)
	if !field.IsValid() {
		return nil, fmt.Errorf("field '%s' not found", finalField)
	}

	return field.Interface(), nil
}

// setNestedFieldUnrestricted sets a nested field without any restrictions
func setNestedFieldUnrestricted(obj interface{}, parts []string, value interface{}) error {
	if len(parts) == 0 {
		return fmt.Errorf("empty field path")
	}

	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("object must be a pointer to a struct")
	}

	v = v.Elem()

	// Navigate to the nested field
	for i, part := range parts[:len(parts)-1] {
		field := v.FieldByName(titleCase(part))
		if !field.IsValid() {
			return fmt.Errorf("field '%s' not found at level %d", part, i)
		}
		if field.Kind() == reflect.Ptr {
			if field.IsNil() {
				return fmt.Errorf("nil pointer encountered at field '%s'", part)
			}
			field = field.Elem()
		}
		if field.Kind() != reflect.Struct {
			return fmt.Errorf("field '%s' is not a struct", part)
		}
		v = field
	}

	// Set the final field
	finalField := titleCase(parts[len(parts)-1])
	field := v.FieldByName(finalField)
	if !field.IsValid() {
		return fmt.Errorf("field '%s' not found", finalField)
	}
	if !field.CanSet() {
		return fmt.Errorf("field '%s' cannot be set", finalField)
	}

	// Convert and set the value
	return setFieldValue(field, value)
}

// deleteNestedField sets a nested field to its zero value
func deleteNestedField(obj interface{}, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("empty field path")
	}

	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("object must be a pointer to a struct")
	}

	v = v.Elem()

	// Navigate to the nested field
	for i, part := range parts[:len(parts)-1] {
		field := v.FieldByName(titleCase(part))
		if !field.IsValid() {
			return fmt.Errorf("field '%s' not found at level %d", part, i)
		}
		if field.Kind() == reflect.Ptr {
			if field.IsNil() {
				return fmt.Errorf("nil pointer encountered at field '%s'", part)
			}
			field = field.Elem()
		}
		if field.Kind() != reflect.Struct {
			return fmt.Errorf("field '%s' is not a struct", part)
		}
		v = field
	}

	// Set the final field to its zero value
	finalField := titleCase(parts[len(parts)-1])
	field := v.FieldByName(finalField)
	if !field.IsValid() {
		return fmt.Errorf("field '%s' not found", finalField)
	}
	if !field.CanSet() {
		return fmt.Errorf("field '%s' cannot be set", finalField)
	}

	// Set to zero value
	zeroValue := reflect.Zero(field.Type())
	field.Set(zeroValue)

	return nil
}

// convertYamlPathToStructPath converts YAML dot notation to Go struct field path
func convertYamlPathToStructPath(yamlPath string) string {
	// Map of YAML field names to Go struct field names
	fieldMap := map[string]string{
		// Top level fields
		"scooter":      "Scooter",
		"environment":  "Environment",
		"mqtt":         "MQTT",
		"ntp":          "NTP",
		"redis_url":    "RedisURL",
		"telemetry":    "Telemetry",
		"commands":     "Commands",
		"service_name": "ServiceName",
		"debug":        "Debug",

		// Scooter fields
		"identifier": "Identifier",
		"token":      "Token",

		// MQTT fields
		"broker_url":       "BrokerURL",
		"ca_cert":          "CACert",
		"ca_cert_embedded": "CACertEmbedded",
		"keep_alive":       "KeepAlive",
		"keepalive":        "KeepAlive", // Alternative naming

		// NTP fields
		"enabled": "Enabled",
		"server":  "Server",

		// Telemetry fields
		"intervals":       "Intervals",
		"priorities":      "Priorities",
		"buffer":          "Buffer",
		"transmit_period": "TransmitPeriod",

		// Telemetry.Priorities fields
		"immediate": "Immediate",
		"quick":     "Quick",
		"medium":    "Medium",
		"slow":      "Slow",

		// Events fields
		"events":      "Events",
		"buffer_path": "BufferPath",

		// Telemetry.Intervals fields
		"driving":            "Driving",
		"standby":            "Standby",
		"standby_no_battery": "StandbyNoBattery",
		"hibernate":          "Hibernate",

		// Telemetry.Buffer fields
		"max_size":       "MaxSize",
		"max_retries":    "MaxRetries",
		"retry_interval": "RetryInterval",
		"persist_path":   "PersistPath",

		// Commands fields (command names)
		"alarm":        "alarm",
		"blinkers":     "blinkers",
		"honk":         "honk",
		"locate":       "locate",
		"lock":         "lock",
		"open_seatbox": "open_seatbox",
		"unlock":       "unlock",

		// Command sub-fields
		"disabled": "Disabled",
		"params":   "Params",
	}

	parts := strings.Split(yamlPath, ".")
	structParts := make([]string, len(parts))

	for i, part := range parts {
		if mapped, exists := fieldMap[part]; exists {
			structParts[i] = mapped
		} else {
			// If no mapping exists, use titleCase as fallback
			structParts[i] = titleCase(part)
		}
	}

	return strings.Join(structParts, ".")
}

// validatePriorityOrdering ensures priority durations are ordered correctly:
// immediate <= quick <= medium <= slow
func validatePriorityOrdering(config *models.Config) error {
	immediate, err := time.ParseDuration(config.Telemetry.Priorities.Immediate)
	if err != nil {
		return nil // Already caught by duration validation
	}
	quick, err := time.ParseDuration(config.Telemetry.Priorities.Quick)
	if err != nil {
		return nil
	}
	medium, err := time.ParseDuration(config.Telemetry.Priorities.Medium)
	if err != nil {
		return nil
	}
	slow, err := time.ParseDuration(config.Telemetry.Priorities.Slow)
	if err != nil {
		return nil
	}

	if immediate > quick {
		return fmt.Errorf("priorities.immediate (%s) must be <= priorities.quick (%s)",
			config.Telemetry.Priorities.Immediate, config.Telemetry.Priorities.Quick)
	}
	if quick > medium {
		return fmt.Errorf("priorities.quick (%s) must be <= priorities.medium (%s)",
			config.Telemetry.Priorities.Quick, config.Telemetry.Priorities.Medium)
	}
	if medium > slow {
		return fmt.Errorf("priorities.medium (%s) must be <= priorities.slow (%s)",
			config.Telemetry.Priorities.Medium, config.Telemetry.Priorities.Slow)
	}

	return nil
}
