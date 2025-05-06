# Radio Gaga

[![Build and Release](https://github.com/rescoot/radio-gaga/actions/workflows/build-and-release.yml/badge.svg)](https://github.com/rescoot/radio-gaga/actions/workflows/build-and-release.yml)

## Overview

Radio Gaga is a telemetry and remote control system designed for managing electric scooters. The application serves as the communication bridge between scooters and cloud services, enabling real-time monitoring and control capabilities.

The system interfaces with Redis for local state management and uses MQTT for secure, efficient communication with cloud services. Radio Gaga intelligently adapts its reporting frequency based on the scooter's operational state, optimizing bandwidth usage while ensuring timely updates during critical operations.

## Key Features

- **Intelligent Telemetry**: Adaptive reporting intervals based on vehicle state (driving, standby, hibernating)
- **Comprehensive State Monitoring**: Tracks vehicle state, engine data, battery information, location, and system status
- **Remote Control Capabilities**: Execute commands remotely via MQTT
- **Secure Communication**: TLS encryption with certificate validation for MQTT
- **Flexible Configuration**: YAML-based configuration with command-line overrides and Redis fallbacks
- **Self-Update Mechanism**: OTA updates with checksum verification and automatic rollback
- **Navigation Support**: Set destination coordinates for navigation (for LibreScoot DBC)
- **Environment-Aware Operation**: Different behavior for development vs. production environments
- **System Integration**: Systemd service management and filesystem handling
- **Development Tools**: Shell and Redis command execution support (development environment only)

## System Architecture

### Components

- **MQTT Client**: Manages secure communication with the MQTT broker, handles connection recovery and message delivery
- **Redis Backend**: Stores and retrieves vehicle state information, provides pub/sub for internal events
- **Telemetry Engine**: Collects and processes vehicle data with adaptive reporting intervals
- **Command Handler**: Processes incoming commands and executes appropriate actions
- **Self-Update System**: Manages OTA updates with verification and rollback capabilities
- **Configuration Management**: YAML-based configuration with fallback mechanisms

### Supported Commands

#### Core Commands
- `ping`: Health check command that verifies connectivity
- `get_state`: Retrieve current comprehensive vehicle telemetry
- `self_update`: Update the radio-gaga client with checksum verification
- `lock`: Lock the scooter
- `unlock`: Unlock the scooter
- `blinkers`: Control turn signals (left/right/both/off)
- `honk`: Activate horn with configurable duration
- `open_seatbox`: Open the seat box
- `locate`: Help locate scooter (flashes lights and honks in a pattern)
- `alarm`: Trigger alarm system with configurable parameters (hazard lights, horn patterns)
- `navigate`: Set destination coordinates for navigation

#### Development Environment Only
- `redis`: Execute Redis commands (get, set, hget, hset, hgetall, lpush, lpop, publish)
- `shell`: Execute shell commands with output streaming capability

## Configuration

### Configuration Methods
1. YAML Configuration File (`radio-gaga.yml`)
2. Command Line Flags (override YAML settings)
3. Redis Settings (fallback for MQTT broker URL and CA certificate)

The system follows a hierarchical configuration approach, with command-line flags taking precedence over YAML configuration, which in turn takes precedence over Redis fallback values.

### Configuration Options
```yaml
scooter:
  identifier: "VEHICLE-ID"    # Vehicle identifier (MQTT username)
  token: "auth_token"         # Authentication token (MQTT password)

environment: "production"     # production or development

mqtt:
  broker_url: "ssl://mqtt.example.com:8883"  # Fallback to Redis `HGET settings cloud:mqtt-url` if not set
  ca_cert: "/path/to/ca.crt"  # Optional CA certificate file path for TLS (fallback to Redis `HGET settings cloud:mqtt-ca` if not set)
  # Alternatively, embed the certificate directly in the config file
  # ca_cert_embedded: |
  #   -----BEGIN CERTIFICATE-----
  #   MIIFTTCCAzWgAwIBAgIBATANBgkqhkiG9w0BAQsFADBIMQswCQYDVQQGEwJERTEa
  #   ... certificate content ...
  #   -----END CERTIFICATE-----
  keepalive: "180s"           # MQTT keepalive interval

ntp:
  enabled: true               # Enable/disable NTP time synchronization
  server: "pool.ntp.rescoot.org"  # NTP server address

redis_url: "redis://localhost:6379"

telemetry:
  intervals:
    driving: "1s"            # Interval while driving
    standby: "5m"            # Interval in standby with battery
    standby_no_battery: "8h" # Interval in standby without battery
    hibernate: "24h"         # Interval in hibernate mode

commands:                    # Optional command configuration
  honk:
    disabled: false          # Commands can be disabled by setting `disabled: true`
    on_time: "100ms"
  alarm:
    hazards:
      flash: true
    horn:
      honk: true
      on_time: "400ms"
      off_time: "400ms"
```

### Command Line Flags
```bash
  -config string
        Path to config file (defaults to radio-gaga.yml)
  -environment string
        Environment (production or development)
  -identifier string
        Vehicle identifier (MQTT username)
  -token string
        Authentication token (MQTT password)
  -mqtt-broker string
        MQTT broker URL
  -mqtt-cacert string
        Path to MQTT CA certificate
  -mqtt-keepalive string
        MQTT keepalive duration (default "30s")
  -redis-url string
        Redis URL (default "redis://localhost:6379")
  -ntp-enabled
        Enable NTP time synchronization (default true)
  -ntp-server string
        NTP server address (default "pool.ntp.rescoot.org")
  -driving-interval string
        Telemetry interval while driving (default "1s")
  -standby-interval string
        Telemetry interval in standby (default "5m")
  -standby-no-battery-interval string
        Telemetry interval in standby without battery (default "8h")
  -hibernate-interval string
        Telemetry interval in hibernate mode (default "24h")
```

## Telemetry System

Radio Gaga features an intelligent telemetry system that adapts its reporting frequency based on the scooter's operational state:

- **Driving Mode**: High-frequency updates (default: every 1 second)
- **Standby Mode**: Moderate-frequency updates (default: every 5 minutes)
- **Standby without Battery**: Low-frequency updates (default: every 8 hours)
- **Hibernate Mode**: Minimal updates (default: every 24 hours)

The system automatically detects state changes and adjusts reporting intervals accordingly. It also sends a final telemetry update before power suspension to ensure the latest state is recorded.

### Telemetry Data Structure

The system collects and reports comprehensive vehicle telemetry including:

#### Vehicle State
- Operating state (ready-to-drive, parked, locked, stand-by, hibernating)
- Handlebar position and lock status
- Kickstand position
- Seatbox lock status
- Blinker status
- Brake status (left/right)
- Horn and seatbox button states

#### Engine Data
- Current speed and motor RPM
- Odometer reading
- Motor voltage and current
- Motor temperature
- Engine state and firmware version
- KERS (Kinetic Energy Recovery System) status

#### Battery Information
- Main batteries (up to 2): presence, charge level (0-100%), voltage, current, temperature, health
- Auxiliary battery (AUX): level, voltage, charge status
- Connectivity Battery Box (CBB): detailed status including cycle count, capacity, charging time

#### System Information
- MDB (Middle Driver Board) version and flavor
- DBC (Dashboard Controller) version and flavor
- Environment information
- Firmware versions

#### Connectivity Status
- Modem state and health
- Internet connection status
- Cloud connection status
- Signal quality and network information
- SIM card details

#### Location Data
- GPS coordinates (latitude/longitude)
- Altitude, speed, and course
- GPS state and timestamp

#### Additional Data
- Power management status
- Bluetooth status
- Keycard reader status
- Dashboard status
- Navigation destination (if set)
 
## Self-Update System

Radio Gaga includes a self-update mechanism that allows for Over-The-Air (OTA) updates with several safety features:

- **Secure Download**: Downloads new binary from specified URL
- **Integrity Verification**: Validates downloaded binary using checksum (SHA-256, SHA-1)
- **Filesystem Handling**: Automatically handles read-only filesystems by remounting when necessary
- **Backup Creation**: Creates backup of current binary before replacement
- **Service Management**: Restarts systemd service after update
- **Automatic Rollback**: Reverts to previous version if update fails or service doesn't start properly
- **Logging**: Comprehensive logging of the update process

The system supports both in-band updates (via MQTT command) and out-of-band updates (via direct script execution).

## Prerequisites

- Go 1.22 or higher
- Redis server (for state management)
- MQTT broker (for cloud communication)

## Building

```bash
# Standard development build
make

# Cross-platform builds
make amd64  # Linux AMD64 build
make arm    # Linux ARM build (ARMv7)

# Distribution build for deployment (optimized ARM build)
make dist

# Debug build with symbols for ARM
make arm-debug
```

The build process automatically embeds version information derived from git tags and commits, making each build uniquely identifiable.

### Automated Builds

Radio Gaga uses GitHub Actions for continuous integration and deployment:

- Every push to the `main` branch and pull requests trigger an ARM build
- When a tag (starting with 'v', e.g., v1.0.0) is pushed, a GitHub Release is automatically created
- Release artifacts include an optimized ARM build named `radio-gaga-[version]-arm`

To create a new release, simply push a tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

## Installation

### Standard Installation

```bash
# Clone the repository
git clone https://github.com/rescoot/radio-gaga.git
cd radio-gaga

# Build the application
make

# Create configuration file (copy and edit example)
cp radio-gaga.example.yml radio-gaga.yml
nano radio-gaga.yml

# Run the application
./radio-gaga
```

### Automated Installation (for unu Scooters)

:warning: This is work in progress.

The repository includes an installation script that automates the setup process for Rescoot scooters. The installer will:
1. Verify the environment is a valid scooter
2. Prompt for authentication token
3. Fetch scooter-specific configuration
4. Download the latest binary
5. Create and enable a systemd service
6. Start the service

## Running

```bash
# Using default config file (radio-gaga.yml)
./radio-gaga

# Using custom config file
./radio-gaga -config /path/to/config.yml

# Override config with flags
./radio-gaga -identifier SCOOTER1 -mqtt-broker ssl://mqtt.example.com:8883 -environment development

# View all available flags
./radio-gaga -help
```

## Security Features

Radio Gaga implements multiple security measures to ensure safe and secure operation:

- **TLS Encryption**: Secure MQTT communication with certificate validation
- **NTP Time Synchronization**: Ensures valid certificate verification by keeping system time accurate
- **Custom CA Certificate Support**: Allows for organization-specific certificate authorities
- **Vehicle-specific Authentication**: Each scooter has unique identifier/token credentials
- **Environment-based Command Restrictions**: Certain commands (shell, redis) are restricted to development environments
- **Command Validation and Sanitization**: All incoming commands are validated before execution
- **Retained Message Cleanup**: Automatically cleans up retained MQTT messages to prevent stale commands
- **Secure Update Process**: Checksum verification for all updates with automatic rollback
- **Filesystem Protection**: Handles read-only filesystems appropriately during updates
- **Secure Defaults**: Conservative default settings prioritize security

## License

[GNU Affero Public License 3.0](LICENSE)

## Authors

- Teal Bauer <teal@rescoot.org>
