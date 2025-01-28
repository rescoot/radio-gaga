# Radio Gaga

## Overview

Radio Gaga is a MQTT-based control and telemetry system designed for managing electric scooters.
The application provides comprehensive remote monitoring and control capabilities by interfacing with Redis for state management and MQTT for communication.

## Key Features

- Real-time telemetry reporting
- Remote scooter control commands
- Configurable telemetry publishing intervals
- Secure MQTT authentication
- Extensive vehicle state tracking

## System Architecture

### Components
- MQTT Client: Manages communication with the MQTT broker
- Redis Backend: Stores and retrieves vehicle state information
- Configuration Management: YAML-based configuration system

### Supported Commands
- `ping`: Health check
- `update`: Trigger system update
- `get_state`: Retrieve current vehicle telemetry
- `lock`: Lock the scooter
- `unlock`: Unlock the scooter
- `blinkers`: Control turn signal state
- `honk`: Activate horn (for 50ms)
- `open_seatbox`: Open storage compartment
- `locate`: Help locate the scooter by flashing lights and honking twice
- `alarm`: Trigger alarm system

## Configuration

Configuration is managed via a YAML file (`radio-gaga.yml`) with the following key sections:

```yaml
vin: VEHICLE-ID               # Unique Vehicle Identification Number
mqtt:
  broker_url: tcp://mqtt.example.com:1883
  token: authentication_token
telemetry:
  check_interval: 100ms       # Frequency of telemetry checks
  min_interval: 1s            # Minimum interval between telemetry publishes
  max_interval: 60s           # Maximum interval between telemetry publishes
redis:
  addr: "localhost:6379"      # Redis server address
  password: ""                # Redis authentication (optional)
```

## Telemetry Data

The system captures comprehensive telemetry including:
- Vehicle state (lock/unlock)
- Kickstand status
- Blinker state
- Speed
- Odometer
- Motor metrics
- Battery levels
- GPS coordinates

## Prerequisites

- Go 1.16+
- Redis
- MQTT Broker
- Required Go Packages:
  - github.com/eclipse/paho.mqtt.golang
  - github.com/go-redis/redis/v8
  - gopkg.in/yaml.v2

## Building

```bash
# Standard build
make build

# Cross-platform builds
make amd64  # Linux AMD64 build
make arm    # Linux ARM build
```

## Running

```bash
# Using default config
./radio-gaga

# Specify custom config
./radio-gaga -config /path/to/custom/config.yml
```

## Security Considerations

- Uses VIN as MQTT authentication
- Configurable MQTT token
- Supports encrypted Redis connections
- Implements command validation
- Graceful error handling

## License

[GNU Affero Public License 3.0](LICENSE)

## Authors

- Teal Bauer <teal@rescoot.org>
