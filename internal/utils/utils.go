package utils

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"hash"
	"io"
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

// ParseIntPtr parses a string to an integer pointer, returns nil if empty
func ParseIntPtr(s string) *int {
	if s == "" {
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &v
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

// QueryNTPTime queries NTP and returns the time without setting the system clock.
// This allows using NTP as a clock reference even without root privileges.
func QueryNTPTime(config *models.NTPConfig) (time.Time, error) {
	if config != nil && config.Enabled == false {
		return time.Time{}, fmt.Errorf("NTP time synchronization is disabled in configuration")
	}

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
		log.Printf("Got NTP time from %s: %s", server, ntpTime.Format(time.RFC3339))
		return ntpTime, nil
	}

	return time.Time{}, fmt.Errorf("failed to query time from any NTP server: %v", lastErr)
}

// SyncTimeNTP attempts to sync the system time using NTP
func SyncTimeNTP(config *models.NTPConfig) error {
	ntpTime, err := QueryNTPTime(config)
	if err != nil {
		return err
	}

	tv := syscall.NsecToTimeval(ntpTime.UnixNano())

	if err := syscall.Settimeofday(&tv); err != nil {
		log.Printf("Warning: failed to set system time: %v (NTP time was valid)", err)
		return nil
	}

	log.Printf("Successfully synchronized system time via NTP")
	return nil
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

// ReadMdbSerialNumbers reads MDB serial numbers from hardware registers.
// It tries the NVMEM device first, then falls back to the OTP sysfs files —
// the same strategy used by version-service.
// Returns the legacy decimal S/N (sum of CFG0+CFG1, unu-compatible) and
// the real hex S/N (CFG1||CFG0, matching the hardware register layout).
func ReadMdbSerialNumbers() (legacySN, realSN string, err error) {
	cfg0, cfg1, err := readIdentifierValues()
	if err != nil {
		return "", "", err
	}
	legacySN = fmt.Sprintf("%d", cfg0+cfg1)
	realSN = fmt.Sprintf("%08x%08x", cfg1, cfg0)
	return legacySN, realSN, nil
}

// readIdentifierValues reads CFG0 and CFG1 as uint64 values.
// Tries NVMEM first (offset 4 for CFG0, offset 8 for CFG1), falls back to OTP sysfs.
func readIdentifierValues() (cfg0, cfg1 uint64, err error) {
	const nvmemPath = "/sys/bus/nvmem/devices/imx-ocotp0/nvmem"
	if _, statErr := os.Stat(nvmemPath); statErr == nil {
		c0, e0 := readUint32FromNvmem(nvmemPath, 4)
		c1, e1 := readUint32FromNvmem(nvmemPath, 8)
		if e0 == nil && e1 == nil {
			return c0, c1, nil
		}
		log.Printf("NVMEM read failed (cfg0: %v, cfg1: %v), falling back to OTP sysfs", e0, e1)
	}

	cfg0, err = readOTPHexFile("/sys/fsl_otp/HW_OCOTP_CFG0")
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read CFG0: %v", err)
	}
	cfg1, err = readOTPHexFile("/sys/fsl_otp/HW_OCOTP_CFG1")
	if err != nil {
		return 0, 0, fmt.Errorf("failed to read CFG1: %v", err)
	}
	return cfg0, cfg1, nil
}

// readUint32FromNvmem reads 4 bytes from the NVMEM device at the given byte offset
// and returns them as a uint64 (little-endian, byte-reversed to match OTP sysfs convention).
func readUint32FromNvmem(path string, offset int) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(f, buf); err != nil {
		return 0, err
	}
	// byte-reverse: NVMEM is little-endian, OTP sysfs gives big-endian
	v := uint64(buf[3])<<24 | uint64(buf[2])<<16 | uint64(buf[1])<<8 | uint64(buf[0])
	return v, nil
}

// ReadMdbSerialNumber reads the legacy decimal MDB serial number (unu-compatible).
func ReadMdbSerialNumber() (string, error) {
	legacy, _, err := ReadMdbSerialNumbers()
	return legacy, err
}

// ParseOTPStrings computes both serial numbers from raw OTP file values read externally
// (e.g. via SSH). Both strings may include or omit the "0x" prefix.
func ParseOTPStrings(cfg0Str, cfg1Str string) (legacySN, realSN string, err error) {
	cfg0, err := parseOTPString(cfg0Str)
	if err != nil {
		return "", "", fmt.Errorf("CFG0: %v", err)
	}
	cfg1, err := parseOTPString(cfg1Str)
	if err != nil {
		return "", "", fmt.Errorf("CFG1: %v", err)
	}
	legacySN = fmt.Sprintf("%d", cfg0+cfg1)
	realSN = fmt.Sprintf("%08x%08x", cfg1, cfg0)
	return legacySN, realSN, nil
}

// readOTPHexFile reads a hex value from an OTP sysfs file (format: "0xABCDEF01").
func readOTPHexFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("cannot open %s: %v", path, err)
	}
	return parseOTPString(strings.TrimSpace(string(data)))
}

// parseOTPString parses a hex string with optional "0x" prefix.
func parseOTPString(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q as hex: %v", s, err)
	}
	return v, nil
}
