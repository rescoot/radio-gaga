package models

import "time"

// CommandLineFlags contains all command-line options
type CommandLineFlags struct {
	ConfigPath    string
	Identifier    string
	Token         string
	MqttBrokerURL string
	MqttCACert    string
	MqttKeepAlive string
	RedisURL      string
	Environment   string
	Debug         bool
	// NTP configuration
	NtpEnabled bool
	NtpServer  string
	// Telemetry intervals
	DrivingInterval          string
	StandbyInterval          string
	StandbyNoBatteryInterval string
	HibernateInterval        string
	// Telemetry buffer options
	BufferEnabled       bool
	BufferMaxSize       int
	BufferMaxRetries    int
	BufferRetryInterval string
	BufferPersistPath   string
	TransmitPeriod      string
}

// Config represents the application configuration
type Config struct {
	Scooter     ScooterConfig      `yaml:"scooter"`
	Environment string             `yaml:"environment"`
	MQTT        MQTTConfig         `yaml:"mqtt"`
	NTP         NTPConfig          `yaml:"ntp"`
	RedisURL    string             `yaml:"redis_url"`
	Telemetry   TelemetryConfig    `yaml:"telemetry"`
	Commands    map[string]Command `yaml:"commands"`
	ServiceName string             `yaml:"service_name,omitempty"`
	Debug       bool               `yaml:"debug,omitempty"`
}

// ScooterConfig contains scooter-specific configuration
type ScooterConfig struct {
	Identifier string `yaml:"identifier"`
	Token      string `yaml:"token"`
}

// MQTTConfig contains MQTT configuration
type MQTTConfig struct {
	BrokerURL string `yaml:"broker_url"`
	CACert    string `yaml:"ca_cert"`
	KeepAlive string `yaml:"keepalive"`
}

// NTPConfig contains NTP time synchronization configuration
type NTPConfig struct {
	Enabled bool   `yaml:"enabled"`
	Server  string `yaml:"server"`
}

// TelemetryConfig contains telemetry configuration
type TelemetryConfig struct {
	Intervals      TelemetryIntervals `yaml:"intervals"`
	Buffer         BufferConfig       `yaml:"buffer,omitempty"`
	TransmitPeriod string             `yaml:"transmit_period,omitempty"`
}

// BufferConfig contains telemetry buffer configuration
type BufferConfig struct {
	Enabled       bool   `yaml:"enabled"`
	MaxSize       int    `yaml:"max_size"`
	MaxRetries    int    `yaml:"max_retries"`
	RetryInterval string `yaml:"retry_interval"`
	PersistPath   string `yaml:"persist_path,omitempty"`
}

// TelemetryIntervals contains telemetry interval settings
type TelemetryIntervals struct {
	Driving          string `yaml:"driving"`
	Standby          string `yaml:"standby"`
	StandbyNoBattery string `yaml:"standby_no_battery"`
	Hibernate        string `yaml:"hibernate"`
}

// Command represents a remote command configuration
type Command struct {
	Disabled bool                   `yaml:"disabled"`
	Params   map[string]interface{} `yaml:"params,omitempty"`
}

// VehicleState represents the current state of the vehicle
type VehicleState struct {
	State         string `json:"state"`
	MainPower     string `json:"main_power"`
	HandlebarLock string `json:"handlebar_lock"`
	HandlebarPos  string `json:"handlebar_position"`
	BrakeLeft     string `json:"brake_left"`
	BrakeRight    string `json:"brake_right"`
	SeatboxLock   string `json:"seatbox"`
	Kickstand     string `json:"kickstand"`
	BlinkerSwitch string `json:"blinker_switch"`
	BlinkerState  string `json:"blinker_state"`
	SeatboxButton string `json:"seatbox_button"`
	HornButton    string `json:"horn_button"`
}

// EngineData represents the engine control unit data
type EngineData struct {
	Speed         int    `json:"speed"`
	Odometer      int    `json:"odometer"`
	MotorVoltage  int    `json:"motor_voltage"`
	MotorCurrent  int    `json:"motor_current"`
	Temperature   int    `json:"temperature"`
	EngineState   string `json:"engine_state"`
	KersState     string `json:"kers_state"`
	KersReasonOff string `json:"kers_reason_off"`
	MotorRPM      int    `json:"motor_rpm"`
	ThrottleState string `json:"throttle_state"`
	EngineFWVer   string `json:"engine_fw_version"`
}

// BatteryData represents data for a main battery
type BatteryData struct {
	Level             int    `json:"level"`
	Present           bool   `json:"present"`
	Voltage           int    `json:"voltage"`
	Current           int    `json:"current"`
	State             string `json:"state"`
	TemperatureState  string `json:"temp_state"`
	SOH               int    `json:"soh"`
	Temps             []int  `json:"temps"`
	CycleCount        int    `json:"cycle_count"`
	FWVersion         string `json:"fw_version"`
	ManufacturingDate string `json:"manufacturing_date"`
	SerialNumber      string `json:"serial_number"`
}

// AuxBatteryData represents data for the auxiliary battery
type AuxBatteryData struct {
	Level        int    `json:"level"`
	Voltage      int    `json:"voltage"`
	ChargeStatus string `json:"charge_status"`
}

// CBBatteryData represents data for the Connectivity Battery Box
type CBBatteryData struct {
	Level             int    `json:"level"`
	Current           int    `json:"current"`
	Temperature       int    `json:"temp"`
	SOH               int    `json:"soh"`
	ChargeStatus      string `json:"charge_status"`
	CellVoltage       int    `json:"cell_voltage"`
	CycleCount        int    `json:"cycle_count"`
	FullCapacity      int    `json:"full_capacity"`
	PartNumber        string `json:"part_number"`
	Present           bool   `json:"present"`
	RemainingCapacity int    `json:"remaining_capacity"`
	SerialNumber      string `json:"serial_number"`
	TimeToEmpty       int    `json:"time_to_empty"`
	TimeToFull        int    `json:"time_to_full"`
	UniqueID          string `json:"unique_id"`
}

// SystemInfo represents system information
type SystemInfo struct {
	MdbVersion   string `json:"mdb_version"`
	Environment  string `json:"environment"`
	NrfFWVersion string `json:"nrf_fw_version"`
	DbcVersion   string `json:"dbc_version"`
	DBCFlavor    string `json:"dbc_flavor,omitempty"`
	MDBFlavor    string `json:"mdb_flavor,omitempty"`
}

// ConnectivityStatus represents internet and modem connectivity
type ConnectivityStatus struct {
	ModemState     string `json:"modem_state"`
	InternetStatus string `json:"internet_status"`
	CloudStatus    string `json:"cloud_status"`
	IPAddress      string `json:"ip_address"`
	AccessTech     string `json:"access_tech"`
	SignalQuality  int    `json:"signal_quality"`
	ModemHealth    string `json:"modem_health,omitempty"`
	SIMIMEI        string `json:"sim_imei,omitempty"`
	SIMIMSI        string `json:"sim_imsi,omitempty"`
	SIMICCID       string `json:"sim_iccid,omitempty"`
}

// ModemData represents detailed modem information
type ModemData struct {
	PowerState       string `json:"power_state,omitempty"`
	SIMState         string `json:"sim_state,omitempty"`
	SIMLock          string `json:"sim_lock,omitempty"`
	OperatorName     string `json:"operator_name,omitempty"`
	OperatorCode     string `json:"operator_code,omitempty"`
	IsRoaming        bool   `json:"is_roaming,omitempty"`
	RegistrationFail string `json:"registration_fail,omitempty"`
}

// GPSData represents GPS position data
type GPSData struct {
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	Altitude  float64 `json:"altitude"`
	GpsSpeed  float64 `json:"gps_speed"`
	Course    float64 `json:"course"`
	State     string  `json:"state,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"`
}

// PowerStatus represents power subsystem status
type PowerStatus struct {
	PowerState    string `json:"power_state"`
	PowerMuxInput string `json:"power_mux_input"`
	WakeupSource  string `json:"wakeup_source"`
}

// BLEStatus represents Bluetooth status
type BLEStatus struct {
	MacAddress string `json:"mac_address"`
	Status     string `json:"status"`
}

// KeycardStatus represents keycard reader status
type KeycardStatus struct {
	Authentication string `json:"authentication"`
	UID            string `json:"uid"`
	Type           string `json:"type"`
}

// DashboardStatus represents dashboard status
type DashboardStatus struct {
	Mode         string `json:"mode"`
	Ready        bool   `json:"ready"`
	SerialNumber string `json:"serial_number"`
}

// NavigationData represents navigation data
type NavigationData struct {
	Destination string `json:"destination,omitempty"`
}

// TelemetryData represents the main telemetry data structure
type TelemetryData struct {
	Version      int                    `json:"version"`
	BuildVersion string                 `json:"build_version,omitempty"`
	Config       map[string]interface{} `json:"config,omitempty"`
	VehicleState VehicleState           `json:"vehicle_state"`
	Engine       EngineData             `json:"engine"`
	Battery0     BatteryData            `json:"battery0"`
	Battery1     BatteryData            `json:"battery1"`
	AuxBattery   AuxBatteryData         `json:"aux_battery"`
	CBBattery    CBBatteryData          `json:"cbb_battery"`
	System       SystemInfo             `json:"system"`
	Connectivity ConnectivityStatus     `json:"connectivity"`
	Modem        ModemData              `json:"modem,omitempty"`
	GPS          GPSData                `json:"gps"`
	Power        PowerStatus            `json:"power"`
	BLE          BLEStatus              `json:"ble"`
	Keycard      KeycardStatus          `json:"keycard"`
	Dashboard    DashboardStatus        `json:"dashboard"`
	Navigation   NavigationData         `json:"navigation,omitempty"`
	Timestamp    string                 `json:"timestamp"`
}

// BufferedTelemetryEvent represents a telemetry event in the buffer
type BufferedTelemetryEvent struct {
	Data      *TelemetryData `json:"data"`
	Timestamp time.Time      `json:"timestamp"`
	Attempts  int            `json:"attempts"`
}

// TelemetryBuffer represents a buffer of telemetry events
type TelemetryBuffer struct {
	Events    []BufferedTelemetryEvent `json:"events"`
	BatchID   string                   `json:"batch_id"`
	CreatedAt time.Time                `json:"created_at"`
}

// TelemetryBatch represents a batch of telemetry events to be sent
type TelemetryBatch struct {
	BatchID   string          `json:"batch_id"`
	Count     int             `json:"count"`
	Events    []TelemetryData `json:"events"`
	Timestamp string          `json:"timestamp"`
}

// CommandMessage represents an incoming command
type CommandMessage struct {
	Command   string                 `json:"command"`
	Params    map[string]interface{} `json:"params"`
	Timestamp int64                  `json:"timestamp"`
	RequestID string                 `json:"request_id"`
	Stream    bool                   `json:"stream,omitempty"` // For shell commands, stream output
}

// CommandResponse represents a command response
type CommandResponse struct {
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	RequestID string `json:"request_id"`
}
