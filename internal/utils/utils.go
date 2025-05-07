package utils

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"hash"
	"log"
	"os"
	"radio-gaga/internal/models"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beevik/ntp"
)

// ParseInt safely parses a string to an integer with default 0
func ParseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// ParseFloat safely parses a string to a float with default 0
func ParseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// IsTLSURL checks if a URL uses TLS
func IsTLSURL(url string) bool {
	return strings.HasPrefix(url, "ssl://") || strings.HasPrefix(url, "tls://")
}

// SyncTimeNTP attempts to sync the system time using NTP
func SyncTimeNTP(config *models.NTPConfig) error {
	// If NTP sync is explicitly disabled, return immediately
	if config != nil && config.Enabled == false {
		log.Println("NTP time synchronization is disabled in configuration")
		return nil
	}

	// Use configured server if available, otherwise use default
	ntpServers := []string{}
	if config != nil && config.Server != "" {
		ntpServers = append(ntpServers, config.Server)
	} else {
		ntpServers = append(ntpServers, "pool.ntp.rescoot.org")
	}

	var lastErr error
	for _, server := range ntpServers {
		ntpTime, err := ntp.Time(server)
		if err != nil {
			lastErr = err
			log.Printf("Failed to get time from %s: %v", server, err)
			continue
		}

		// Convert time to timeval
		tv := syscall.NsecToTimeval(ntpTime.UnixNano())

		// Set system time (requires root privileges)
		if err := syscall.Settimeofday(&tv); err != nil {
			lastErr = fmt.Errorf("failed to set system time: %v", err)
			log.Printf("Warning: %v", lastErr)
			// Even if we can't set the system time, return success
			// as we at least got a valid time from NTP
			return nil
		}

		log.Printf("Successfully synchronized time with %s", server)
		return nil
	}

	return fmt.Errorf("failed to sync time with any NTP server: %v", lastErr)
}

// CreateInsecureTLSConfig creates a TLS config that skips verification
func CreateInsecureTLSConfig(caCertPath string) (*tls.Config, error) {
	tlsConfig := new(tls.Config)
	tlsConfig.InsecureSkipVerify = true

	// If we have a CA cert, still load it for basic verification
	if caCertPath != "" {
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %v", err)
		}

		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}

		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

// CreateInsecureTLSConfigWithEmbeddedCert creates a TLS config that skips verification using an embedded certificate
func CreateInsecureTLSConfigWithEmbeddedCert(caCertEmbedded string) (*tls.Config, error) {
	tlsConfig := new(tls.Config)
	tlsConfig.InsecureSkipVerify = true

	// If we have an embedded CA cert, use it for basic verification
	if caCertEmbedded != "" {
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM([]byte(caCertEmbedded)); !ok {
			return nil, fmt.Errorf("failed to parse embedded CA certificate")
		}

		tlsConfig.RootCAs = caCertPool
	}

	return tlsConfig, nil
}

// CreateHash creates a hash based on the algorithm name
func CreateHash(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha256":
		return sha256.New(), nil
	case "sha1":
		return sha1.New(), nil
	default:
		return nil, fmt.Errorf("unsupported checksum algorithm: %s (supported: sha256, sha1)", algorithm)
	}
}

// ParseDuration parses duration from different types
func ParseDuration(value interface{}) (time.Duration, error) {
	switch v := value.(type) {
	case string:
		return time.ParseDuration(v)
	case float64:
		return time.Duration(v) * time.Second, nil
	default:
		return 0, fmt.Errorf("invalid duration type: %T", value)
	}
}

// ConvertToStringKeyMap recursively converts a map with interface{} keys to string keys
// This is necessary for converting YAML-decoded maps to JSON-compatible maps
func ConvertToStringKeyMap(m interface{}) interface{} {
	switch x := m.(type) {
	case map[interface{}]interface{}:
		// Convert to map with string keys
		stringMap := make(map[string]interface{})
		for k, v := range x {
			// Use reflect to safely convert any potential key type to string
			kStr := fmt.Sprintf("%v", k)
			stringMap[kStr] = ConvertToStringKeyMap(v)
		}
		return stringMap
	case map[string]interface{}:
		// Already has string keys, but values might need conversion
		for k, v := range x {
			x[k] = ConvertToStringKeyMap(v)
		}
		return x
	case []interface{}:
		// Convert slice elements
		for i, v := range x {
			x[i] = ConvertToStringKeyMap(v)
		}
		return x
	default:
		// Other types (string, int, etc.) can be returned as is
		return m
	}
}

// ReadMdbSerialNumber reads the MDB serial number from the system
func ReadMdbSerialNumber() (string, error) {
	// Read the first value
	cfg0, err := readHexValueFromFile("/sys/fsl_otp/HW_OCOTP_CFG0")
	if err != nil {
		return "", fmt.Errorf("failed to read MDB serial number part 1: %v", err)
	}

	// Read the second value
	cfg1, err := readHexValueFromFile("/sys/fsl_otp/HW_OCOTP_CFG1")
	if err != nil {
		return "", fmt.Errorf("failed to read MDB serial number part 2: %v", err)
	}

	// Combine the values
	sn := cfg0 + cfg1
	return fmt.Sprintf("%d", sn), nil
}

// readHexValueFromFile reads a hexadecimal value from a file
func readHexValueFromFile(path string) (uint64, error) {
	// Read the file
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("cannot open %s: %v", path, err)
	}

	// Parse the hexadecimal value
	var value uint64
	_, err = fmt.Sscanf(string(data), "0x%x", &value)
	if err != nil {
		return 0, fmt.Errorf("cannot read value from %s: %v", path, err)
	}

	return value, nil
}
