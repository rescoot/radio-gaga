package telemetry

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/go-redis/redis/v8"
	"gopkg.in/yaml.v2"

	"radio-gaga/internal/models"
	"radio-gaga/internal/utils"
)

// GetTelemetryInterval returns the appropriate telemetry interval based on vehicle state
func GetTelemetryInterval(ctx context.Context, redisClient *redis.Client, config *models.Config) (time.Duration, string) {
	// Get current vehicle state from Redis
	vehicle, err := redisClient.HGet(ctx, "vehicle", "state").Result()
	if err != nil {
		log.Printf("Failed to get vehicle state: %v", err)
		return time.Minute, "fallback" // Default fallback
	}

	// Check battery state
	battery0Charge := 0
	battery0Present := false

	battery0, err := redisClient.HGetAll(ctx, "battery:0").Result()
	if err == nil {
		battery0Present = battery0["present"] == "true"
		if charge, err := strconv.Atoi(battery0["charge"]); err == nil {
			battery0Charge = charge
		}
	}

	// Determine interval based on state
	var intervalStr string
	var reason string

	switch vehicle {
	case "ready-to-drive":
		intervalStr = config.Telemetry.Intervals.Driving
		reason = "driving mode"
	case "hibernating":
		// should set a wakeup trigger, is this possible outside librescoot?
		intervalStr = config.Telemetry.Intervals.Hibernate
		reason = "hibernate mode"
	case "parked", "locked", "stand-by":
		if battery0Present && battery0Charge > 0 {
			intervalStr = config.Telemetry.Intervals.Standby
			reason = "standby mode with charged main battery"
		} else {
			intervalStr = config.Telemetry.Intervals.StandbyNoBattery
			reason = "standby mode without battery or with empty battery"
		}
	default:
		intervalStr = config.Telemetry.Intervals.Standby
		reason = "default standby mode"
	}

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		log.Printf("Failed to parse interval %s: %v", intervalStr, err)
		return time.Minute, "fallback" // Default fallback
	}

	return interval, reason
}

// GetBatteryData retrieves battery data from Redis
func GetBatteryData(ctx context.Context, redisClient *redis.Client, index int) (models.BatteryData, error) {
	battery, err := redisClient.HGetAll(ctx, fmt.Sprintf("battery:%d", index)).Result()
	if err != nil {
		return models.BatteryData{}, fmt.Errorf("failed to get battery %d data: %v", index, err)
	}

	temps := make([]int, 4)
	for i := 0; i < 4; i++ {
		temps[i] = utils.ParseInt(battery[fmt.Sprintf("temperature:%d", i)])
	}

	return models.BatteryData{
		Level:             utils.ParseInt(battery["charge"]),
		Present:           battery["present"] == "true",
		Voltage:           utils.ParseInt(battery["voltage"]),
		Current:           utils.ParseInt(battery["current"]),
		State:             battery["state"],
		TemperatureState:  battery["temperature-state"],
		SOH:               utils.ParseInt(battery["state-of-health"]),
		Temps:             temps,
		CycleCount:        utils.ParseInt(battery["cycle-count"]),
		FWVersion:         battery["fw-version"],
		ManufacturingDate: battery["manufacturing-date"],
		SerialNumber:      battery["serial-number"],
	}, nil
}

// GetTelemetryFromRedis retrieves telemetry data from Redis
func GetTelemetryFromRedis(ctx context.Context, redisClient *redis.Client, config *models.Config, version string) (*models.TelemetryData, error) {
	// Create a map from the config struct, excluding the token
	configMap := make(map[string]interface{})
	configBytes, _ := yaml.Marshal(config)
	yaml.Unmarshal(configBytes, &configMap)

	// Convert map with interface{} keys to string keys for JSON compatibility
	configMap = utils.ConvertToStringKeyMap(configMap).(map[string]interface{})

	// Explicitly remove the token (should now only have string keys)
	if scooterConfig, ok := configMap["scooter"].(map[string]interface{}); ok {
		delete(scooterConfig, "token")
	}

	telemetry := &models.TelemetryData{
		Version:      2,
		BuildVersion: version,
		Config:       configMap,
	}

	// Get vehicle state
	vehicle, err := redisClient.HGetAll(ctx, "vehicle").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get vehicle state: %v", err)
	}
	telemetry.VehicleState = models.VehicleState{
		State:         vehicle["state"],
		Kickstand:     vehicle["kickstand"],
		SeatboxLock:   vehicle["seatbox:lock"],
		BlinkerSwitch: vehicle["blinker:switch"],
		HandlebarLock: vehicle["handlebar:lock-sensor"],
		HandlebarPos:  vehicle["handlebar:position"],
		MainPower:     vehicle["main-power"],
		SeatboxButton: vehicle["seatbox:button"],
		HornButton:    vehicle["horn:button"],
		BrakeLeft:     vehicle["brake:left"],
		BrakeRight:    vehicle["brake:right"],
		BlinkerState:  vehicle["blinker:state"],
	}

	// Get engine ECU data
	engineEcu, err := redisClient.HGetAll(ctx, "engine-ecu").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get engine ECU data: %v", err)
	}
	telemetry.Engine = models.EngineData{
		Speed:         utils.ParseInt(engineEcu["speed"]),
		Odometer:      utils.ParseInt(engineEcu["odometer"]),
		MotorVoltage:  utils.ParseInt(engineEcu["motor:voltage"]),
		MotorCurrent:  utils.ParseInt(engineEcu["motor:current"]),
		Temperature:   utils.ParseInt(engineEcu["temperature"]),
		EngineState:   engineEcu["state"],
		KersState:     engineEcu["kers"],
		KersReasonOff: engineEcu["kers-reason-off"],
		MotorRPM:      utils.ParseInt(engineEcu["rpm"]),
		ThrottleState: engineEcu["throttle"],
		EngineFWVer:   engineEcu["fw-version"],
	}

	// Get battery data
	telemetry.Battery0, err = GetBatteryData(ctx, redisClient, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 0 data: %v", err)
	}

	telemetry.Battery1, err = GetBatteryData(ctx, redisClient, 1)
	if err != nil {
		return nil, fmt.Errorf("failed to get battery 1 data: %v", err)
	}

	// Get auxiliary battery data
	auxBattery, err := redisClient.HGetAll(ctx, "aux-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get aux battery data: %v", err)
	}
	telemetry.AuxBattery = models.AuxBatteryData{
		Level:        utils.ParseInt(auxBattery["charge"]),
		Voltage:      utils.ParseInt(auxBattery["voltage"]),
		ChargeStatus: auxBattery["charge-status"],
	}

	// Get CBB data
	cbbBattery, err := redisClient.HGetAll(ctx, "cb-battery").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get CBB data: %v", err)
	}
	telemetry.CBBattery = models.CBBatteryData{
		Level:             utils.ParseInt(cbbBattery["charge"]),
		Current:           utils.ParseInt(cbbBattery["current"]),
		Temperature:       utils.ParseInt(cbbBattery["temperature"]),
		SOH:               utils.ParseInt(cbbBattery["state-of-health"]),
		ChargeStatus:      cbbBattery["charge-status"],
		CellVoltage:       utils.ParseInt(cbbBattery["cell-voltage"]),
		CycleCount:        utils.ParseInt(cbbBattery["cycle-count"]),
		FullCapacity:      utils.ParseInt(cbbBattery["full-capacity"]),
		PartNumber:        cbbBattery["part-number"],
		Present:           cbbBattery["present"] == "true",
		RemainingCapacity: utils.ParseInt(cbbBattery["remaining-capacity"]),
		SerialNumber:      cbbBattery["serial-number"],
		TimeToEmpty:       utils.ParseInt(cbbBattery["time-to-empty"]),
		TimeToFull:        utils.ParseInt(cbbBattery["time-to-full"]),
		UniqueID:          cbbBattery["unique-id"],
	}

	// Get system information
	system, err := redisClient.HGetAll(ctx, "system").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %v", err)
	}
	telemetry.System = models.SystemInfo{
		Environment:  system["environment"],
		DbcVersion:   system["dbc-version"],
		MdbVersion:   system["mdb-version"],
		NrfFWVersion: system["nrf-fw-version"],
		DBCFlavor:    system["dbc-flavor"],
		MDBFlavor:    system["mdb-flavor"],
	}

	// Get internet connectivity status
	internet, err := redisClient.HGetAll(ctx, "internet").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get internet status: %v", err)
	}
	telemetry.Connectivity = models.ConnectivityStatus{
		ModemState:     internet["modem-state"],
		AccessTech:     internet["access-tech"],
		SignalQuality:  utils.ParseInt(internet["signal-quality"]),
		InternetStatus: internet["status"],
		IPAddress:      internet["ip-address"],
		CloudStatus:    internet["unu-cloud"],
		ModemHealth:    internet["modem-health"],
		SIMIMEI:        internet["sim-imei"],
		SIMIMSI:        internet["sim-imsi"],
		SIMICCID:       internet["sim-iccid"],
	}

	modem, err := redisClient.HGetAll(ctx, "modem").Result()
	if err != nil && err != redis.Nil {
		log.Printf("Warning: Failed to get modem data: %v", err)
	} else if err == nil {
		telemetry.Modem = models.ModemData{
			PowerState:       modem["power-state"],
			SIMState:         modem["sim-state"],
			SIMLock:          modem["sim-lock"],
			OperatorName:     modem["operator-name"],
			OperatorCode:     modem["operator-code"],
			IsRoaming:        modem["is-roaming"] == "true",
			RegistrationFail: modem["registration-fail"],
		}
	}

	// Get GPS data
	gps, err := redisClient.HGetAll(ctx, "gps").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get GPS data: %v", err)
	}
	telemetry.GPS = models.GPSData{
		Lat:       utils.ParseFloat(gps["latitude"]),
		Lng:       utils.ParseFloat(gps["longitude"]),
		Altitude:  utils.ParseFloat(gps["altitude"]),
		GpsSpeed:  utils.ParseFloat(gps["speed"]),
		Course:    utils.ParseFloat(gps["course"]),
		State:     gps["state"],
		Timestamp: gps["timestamp"],
	}

	// Get power management and mux status
	powerManager, err := redisClient.HGetAll(ctx, "power-manager").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get power manager status: %v", err)
	}
	powerMux, err := redisClient.HGetAll(ctx, "power-mux").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get power mux status: %v", err)
	}
	telemetry.Power = models.PowerStatus{
		PowerState:    powerManager["state"],
		PowerMuxInput: powerMux["selected-input"],
		WakeupSource:  powerManager["wakeup-source"],
	}

	// Get BLE status
	ble, err := redisClient.HGetAll(ctx, "ble").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get BLE status: %v", err)
	}
	telemetry.BLE = models.BLEStatus{
		MacAddress: ble["mac-address"],
		Status:     ble["status"],
	}

	// Get keycard status (usually not populated - expires 10s after tap)
	keycard, err := redisClient.HGetAll(ctx, "keycard").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get keycard status: %v", err)
	}
	telemetry.Keycard = models.KeycardStatus{
		Authentication: keycard["authentication"],
		UID:            keycard["uid"],
		Type:           keycard["type"],
	}

	// Get dashboard status
	dashboard, err := redisClient.HGetAll(ctx, "dashboard").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get dashboard status: %v", err)
	}
	telemetry.Dashboard = models.DashboardStatus{
		Mode:         dashboard["mode"],
		Ready:        dashboard["ready"] == "true",
		SerialNumber: dashboard["serial-number"],
	}

	// Get navigation data
	navDest, err := redisClient.HGet(ctx, "navigation", "destination").Result()
	if err != nil && err != redis.Nil {
		log.Printf("Warning: Failed to get navigation destination: %v", err)
	} else if err == nil {
		telemetry.Navigation = models.NavigationData{
			Destination: navDest,
		}
	}

	telemetry.Timestamp = time.Now().UTC().Format(time.RFC3339)

	return telemetry, nil
}
