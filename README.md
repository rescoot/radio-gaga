# Radio Gaga

[![Build and Release](https://github.com/rescoot/radio-gaga/actions/workflows/build-and-release.yml/badge.svg)](https://github.com/rescoot/radio-gaga/actions/workflows/build-and-release.yml)

## Overview

Radio Gaga is a telemetry and remote control system designed for managing electric scooters.
The application provides comprehensive remote monitoring and control capabilities by interfacing with Redis for state management and MQTT for communication.

## Key Features

- Real-time telemetry monitoring with adaptive reporting intervals
- Comprehensive vehicle state tracking
- Remote control capabilities via MQTT commands
- TLS encryption support for MQTT communication
- Redis-based state management
- Configurable command parameters
- Support for development and production environments
- Development tools: Shell and Redis command execution support (development environment only)

## System Architecture

### Components

- MQTT Client: Manages communication with the MQTT broker
- Redis Backend: Stores and retrieves vehicle state information
- Configuration Management: YAML-based configuration system

### Supported Commands

- `ping`: Health check command
- `get_state`: Retrieve current vehicle telemetry
- `update`: Update telemetry client
- `lock`: Lock the scooter
- `unlock`: Unlock the scooter
- `blinkers`: Control turn signals (left/right/both/off)
- `honk`: Activate horn with configurable duration
- `open_seatbox`: Open the seat box
- `locate`: Help locate scooter (flashes lights and honks twice in rapid succession)
- `alarm`: Trigger alarm system with configurable parameters

In development environment:

- `redis`: Execute Redis commands (development only)
- `shell`: Execute shell commands (development only)

## Configuration

### Configuration Methods
1. YAML Configuration File (`radio-gaga.yml`)
2. Command Line Flags
3. Redis Settings (fallback for MQTT broker URL and CA certificate)

### Configuration Options
```yaml
scooter:
  identifier: "VEHICLE-ID"    # Vehicle identifier (MQTT username)
  token: "auth_token"         # Authentication token (MQTT password)

environment: "production"     # production or development

mqtt:
  broker_url: "ssl://mqtt.example.com:8883"  # Fallback to Redis `HGET settings cloud:mqtt-url` if not set
  ca_cert: "/path/to/ca.crt"  # Optional CA certificate for TLS (fallback to Redis `HGET settings cloud:mqtt-ca` if not set)
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

## Telemetry Data

The system monitors and reports comprehensive vehicle telemetry including:

### Vehicle State

- Operating state
- Kickstand position
- Seatbox lock status
- Blinker status

### Engine Data

- Current speed
- Odometer reading
- Motor voltage and current
- Motor temperature

### Battery Information

- Main battery presence and levels (0-100%)
- AUX battery level and voltage
- CBB battery level and current

### Location Data

- GPS coordinates (latitude/longitude)
 
## Prerequisites

- Go 1.22 or higher
- Redis server
- MQTT broker

## Building

```bash
# Standard build
make

# Cross-platform builds
make amd64  # Linux AMD64 build
make arm    # Linux ARM build

# Distribution build for deployment
make dist
```

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

## Running

```bash
# Using default config file (radio-gaga.yml)
./radio-gaga

# Using custom config file
./radio-gaga -config /path/to/config.yml

# Override config with flags (run with -help to see flags)
./radio-gaga -identifier SCOOTER1 -mqtt-broker ssl://mqtt.example.com:8883
```

## Security Features

- TLS encryption support for MQTT communication
- Custom CA certificate support for MQTT
- Vehicle-specific authentication using identifier/token
- Environment-based command restrictions
- Command validation and sanitization
- Retained message cleanup
- Secure defaults

## License

[GNU Affero Public License 3.0](LICENSE)

## Authors

- Teal Bauer <teal@rescoot.org>
