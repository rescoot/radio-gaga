package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/go-redis/redis/v8"

	"radio-gaga/internal/handlers/commands"
	"radio-gaga/internal/models"
	"radio-gaga/internal/telemetry"
	"radio-gaga/internal/utils"
)

// CommandHandlerClient is the interface needed by command handlers
type CommandHandlerClient interface {
	SendCommandResponse(requestID, status, errorMsg string)
	SendCommandResponseWithPID(requestID, status, errorMsg string, pid int)
	GetCommandParam(cmd, param string, defaultValue interface{}) interface{}
	CleanRetainedMessage(topic string) error
	PublishTelemetryData(current *models.TelemetryData) error
	GetConfigPath() string
}

// ClientImplementation implements CommandHandlerClient and provides access to client methods
type ClientImplementation struct {
	Config      *models.Config
	ConfigPath  string
	MQTTClient  mqtt.Client
	RedisClient *redis.Client
	Ctx         context.Context
	Version     string
}

// SendCommandResponse sends a response to a command
func (c *ClientImplementation) SendCommandResponse(requestID, status, errorMsg string) {
	c.SendCommandResponseWithPID(requestID, status, errorMsg, 0)
}

// SendCommandResponseWithPID sends a response to a command including process ID
func (c *ClientImplementation) SendCommandResponseWithPID(requestID, status, errorMsg string, pid int) {
	response := models.CommandResponse{
		Status:    status,
		Error:     errorMsg,
		RequestID: requestID,
	}
	
	if pid > 0 {
		response.PID = pid
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal response: %v", err)
		return
	}

	topic := fmt.Sprintf("scooters/%s/acks", c.Config.Scooter.Identifier)
	if token := c.MQTTClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish response: %v", token.Error())
	}

	log.Printf("Published response to %s: %s", topic, string(responseJSON))
}

// GetCommandParam retrieves a command parameter from configuration
func (c *ClientImplementation) GetCommandParam(cmd, param string, defaultValue interface{}) interface{} {
	if cmdConfig, ok := c.Config.Commands[cmd]; ok {
		if params, ok := cmdConfig.Params[param]; ok {
			return params
		}
	}
	return defaultValue
}

// CleanRetainedMessage removes a retained message by publishing an empty payload
func (c *ClientImplementation) CleanRetainedMessage(topic string) error {
	log.Printf("Attempting to clean retained message on topic: %s", topic)

	emptyPayload := []byte{}
	log.Printf("Publishing empty payload with retain=true to topic %s", topic)

	token := c.MQTTClient.Publish(topic, 1, true, emptyPayload)
	token.Wait()

	if err := token.Error(); err != nil {
		log.Printf("MQTT publish token error details: %+v", token)
		log.Printf("MQTT client connection status: %v", c.MQTTClient.IsConnectionOpen())
		log.Printf("Failed to clean retained message. Topic: %s, Error: %v", topic, err)
		return fmt.Errorf("failed to clean retained message: %v", err)
	}

	log.Printf("Successfully cleaned retained message on topic: %s", topic)
	return nil
}

// PublishTelemetryData publishes a telemetry payload to MQTT
func (c *ClientImplementation) PublishTelemetryData(current *models.TelemetryData) error {
	telemetryJSON, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("failed to marshal telemetry: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/telemetry", c.Config.Scooter.Identifier)
	if token := c.MQTTClient.Publish(topic, 1, false, telemetryJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish telemetry: %v", token.Error())
	}

	log.Printf("Published telemetry to %s", topic)
	return nil
}

// GetConfigPath returns the configuration file path
func (c *ClientImplementation) GetConfigPath() string {
	return c.ConfigPath
}

// HandleCommand processes incoming MQTT commands
func HandleCommand(client CommandHandlerClient, mqttClient mqtt.Client, redisClient *redis.Client, ctx context.Context, config *models.Config, version string, mqttMsg mqtt.Message) {
	// Check for empty payload early
	if len(mqttMsg.Payload()) == 0 {
		log.Printf("Received empty payload message on topic: %s, retained: %v",
			mqttMsg.Topic(), mqttMsg.Retained())

		return
	}

	var command models.CommandMessage
	if err := json.Unmarshal(mqttMsg.Payload(), &command); err != nil {
		log.Printf("Failed to parse command: %v", err)
		log.Printf("Payload was %v", mqttMsg.Payload())

		client.SendCommandResponse("unknown", "error", "Invalid command format")
		client.CleanRetainedMessage(mqttMsg.Topic())
		return
	}

	log.Printf("Received command: %s with requestID: %s", command.Command, command.RequestID)

	// Check if command is disabled (except for ping and get_state)
	if command.Command != "ping" && command.Command != "get_state" {
		if cmdConfig, ok := config.Commands[command.Command]; ok && cmdConfig.Disabled {
			log.Printf("Command %s is disabled in config", command.Command)
			client.SendCommandResponse(command.RequestID, "error", "Command disabled in config")
			return
		}
	}

	// Shell command is restricted to development environment
	if command.Command == "shell" && config.Environment != "development" {
		log.Printf("Command %s is not allowed in %s environment", command.Command, config.Environment)
		client.SendCommandResponse(command.RequestID, "error", "Command not allowed in this environment")
		return
	}

	var err error
	switch command.Command {
	case "ping":
		client.SendCommandResponse(command.RequestID, "success", "")
		if mqttMsg.Retained() {
			if err := client.CleanRetainedMessage(mqttMsg.Topic()); err != nil {
				log.Printf("Failed to clean retained message: %v", err)
			}
		}
		return // Skip error handling
	case "get_state":
		err = handleGetStateCommand(client, redisClient, ctx, config, version)
	case "self_update":
		err = handleSelfUpdateCommand(client, command.Params, command.RequestID, config)
	case "lock":
		err = handleLockCommand(redisClient, ctx)
	case "unlock":
		err = handleUnlockCommand(redisClient, ctx)
	case "blinkers":
		err = handleBlinkersCommand(redisClient, ctx, command.Params)
	case "honk":
		err = handleHonkCommand(client, redisClient, ctx)
	case "open_seatbox":
		err = handleSeatboxCommand(redisClient, ctx)
	case "locate":
		err = handleLocateCommand(client, redisClient, ctx)
	case "alarm":
		err = handleAlarmCommand(client, redisClient, ctx, command.Params)
	case "redis":
		err = handleRedisCommand(client, redisClient, ctx, mqttClient, config, command.Params, command.RequestID)
	case "shell":
		err = handleShellCommand(client, mqttClient, config, command.Params, command.RequestID, command.Stream)
	case "navigate":
		err = handleNavigateCommand(redisClient, ctx, command.Params, command.RequestID)
	case "hibernate":
		err = handleHibernateCommand(redisClient, ctx)
	case "keycards:list":
		err = commands.HandleKeycardsListCommand(client, mqttClient, config, command.RequestID)
	case "keycards:add":
		err = commands.HandleKeycardsAddCommand(command.Params)
	case "keycards:delete":
		err = commands.HandleKeycardsDeleteCommand(command.Params)
	case "keycards:master_key:get":
		err = commands.HandleKeycardsMasterKeyGetCommand(client, mqttClient, config, command.RequestID)
	case "keycards:master_key:set":
		err = commands.HandleKeycardsMasterKeySetCommand(command.Params)
	case "config:get":
		err = commands.HandleConfigGetCommand(client, mqttClient, config, command.Params, command.RequestID)
	case "config:set":
		err = commands.HandleConfigSetCommand(client, mqttClient, config, command.Params, command.RequestID)
	case "config:del":
		err = commands.HandleConfigDelCommand(client, mqttClient, config, command.Params, command.RequestID)
	case "config:save":
		err = commands.HandleConfigSaveCommand(client, mqttClient, config, command.RequestID)
	default:
		err = fmt.Errorf("unknown command: %s", command.Command)
	}

	if err != nil {
		log.Printf("Command failed: %v", err)
		client.SendCommandResponse(command.RequestID, "error", err.Error())
		if err := client.CleanRetainedMessage(mqttMsg.Topic()); err != nil {
			log.Printf("Failed to clean message: %v", err)
		}
		return
	}

	if err := client.CleanRetainedMessage(mqttMsg.Topic()); err != nil {
		log.Printf("Failed to clean message: %v", err)
	}

	client.SendCommandResponse(command.RequestID, "success", "")
}

// handleGetStateCommand handles the get_state command
func handleGetStateCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, config *models.Config, version string) error {
	// For immediate get_state requests, use current time as service start time fallback
	// This means get_state will always use absolute timestamps since it's immediate
	serviceStartTime := time.Now().UTC()
	telemetryData, err := telemetry.GetTelemetryFromRedis(ctx, redisClient, config, version, serviceStartTime)
	if err != nil {
		return err
	}

	return client.PublishTelemetryData(telemetryData)
}

// handleLockCommand handles the lock command
func handleLockCommand(redisClient *redis.Client, ctx context.Context) error {
	return redisClient.LPush(ctx, "scooter:state", "lock").Err()
}

// handleUnlockCommand handles the unlock command
func handleUnlockCommand(redisClient *redis.Client, ctx context.Context) error {
	return redisClient.LPush(ctx, "scooter:state", "unlock").Err()
}

// handleBlinkersCommand handles the blinkers command
func handleBlinkersCommand(redisClient *redis.Client, ctx context.Context, params map[string]interface{}) error {
	state, ok := params["state"].(string)
	if !ok {
		return fmt.Errorf("invalid blinker state")
	}

	validStates := map[string]bool{"left": true, "right": true, "both": true, "off": true}
	if !validStates[state] {
		return fmt.Errorf("invalid blinker state: %s", state)
	}

	return redisClient.LPush(ctx, "scooter:blinker", state).Err()
}

// handleHonkCommand handles the honk command
func handleHonkCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context) error {
	onTime := client.GetCommandParam("honk", "on_time", "100ms")
	duration, err := time.ParseDuration(onTime.(string))
	if err != nil {
		duration = 100 * time.Millisecond // Default value
	}

	err = redisClient.LPush(ctx, "scooter:horn", "on").Err()
	if err != nil {
		return err
	}

	time.Sleep(duration)
	return redisClient.LPush(ctx, "scooter:horn", "off").Err()
}

// handleSeatboxCommand handles the open_seatbox command
func handleSeatboxCommand(redisClient *redis.Client, ctx context.Context) error {
	return redisClient.LPush(ctx, "scooter:seatbox", "open").Err()
}

// handleLocateCommand handles the locate command
func handleLocateCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context) error {
	// Turn on blinkers
	param_honk_time := client.GetCommandParam("locate", "honk_time", "40ms")
	param_honk_interval := client.GetCommandParam("locate", "honk_interval", "80ms")
	param_interval := client.GetCommandParam("locate", "interval", "4s")

	honk_time, err := time.ParseDuration(param_honk_time.(string))
	if err != nil {
		honk_time = 40 * time.Millisecond // Default value
	}
	honk_interval, err := time.ParseDuration(param_honk_interval.(string))
	if err != nil {
		honk_interval = 80 * time.Millisecond // Default value
	}
	interval, err := time.ParseDuration(param_interval.(string))
	if err != nil {
		interval = 4 * time.Second
	}

	err = redisClient.LPush(ctx, "scooter:blinker", "both").Err()
	if err != nil {
		return err
	}

	// Honk twice
	honkHorn(redisClient, ctx, honk_time)
	time.Sleep(honk_interval)
	honkHorn(redisClient, ctx, honk_time)

	time.Sleep(interval)

	honkHorn(redisClient, ctx, honk_time)
	time.Sleep(honk_interval)
	honkHorn(redisClient, ctx, honk_time)

	// Turn off blinkers
	return redisClient.LPush(ctx, "scooter:blinker", "off").Err()
}

// honkHorn turns the horn on for a duration and then off
func honkHorn(redisClient *redis.Client, ctx context.Context, duration time.Duration) error {
	err := redisClient.LPush(ctx, "scooter:horn", "on").Err()
	if err != nil {
		return err
	}

	time.Sleep(duration)

	return redisClient.LPush(ctx, "scooter:horn", "off").Err()
}

// handleAlarmCommand handles the alarm command
func handleAlarmCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, params map[string]interface{}) error {
	if state, ok := params["state"].(string); ok && state == "off" {
		return stopAlarm(redisClient, ctx)
	}

	duration, err := utils.ParseDuration(params["duration"])
	if err != nil {
		return fmt.Errorf("invalid alarm duration: %v", err)
	}

	flashHazards := client.GetCommandParam("alarm", "hazards.flash", true).(bool)
	honkHorn := client.GetCommandParam("alarm", "horn.honk", true).(bool)
	hornOnTime := client.GetCommandParam("alarm", "horn.on_time", "400ms").(string)
	hornOffTime := client.GetCommandParam("alarm", "horn.off_time", "400ms").(string)

	return startAlarmWithConfig(redisClient, ctx, duration, flashHazards, honkHorn, hornOnTime, hornOffTime)
}

// startAlarmWithConfig starts the alarm with the given configuration
func startAlarmWithConfig(redisClient *redis.Client, ctx context.Context, duration time.Duration, flashHazards, honkHornEnabled bool, hornOnTime, hornOffTime string) error {
	if flashHazards {
		if err := redisClient.LPush(ctx, "scooter:blinker", "both").Err(); err != nil {
			return err
		}
	}

	if honkHornEnabled {
		onDuration, _ := time.ParseDuration(hornOnTime)
		offDuration, _ := time.ParseDuration(hornOffTime)
		ticker := time.NewTicker(onDuration + offDuration)
		done := make(chan bool)

		go func() {
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					honkHorn(redisClient, ctx, onDuration)
					time.Sleep(offDuration)
				}
			}
		}()

		time.Sleep(duration)
		ticker.Stop()
		done <- true
	} else {
		time.Sleep(duration)
	}

	return stopAlarm(redisClient, ctx)
}

// stopAlarm stops the alarm
func stopAlarm(redisClient *redis.Client, ctx context.Context) error {
	// Turn off blinkers
	err := redisClient.LPush(ctx, "scooter:blinker", "off").Err()
	if err != nil {
		return err
	}

	// Turn off horn
	return redisClient.LPush(ctx, "scooter:horn", "off").Err()
}

// handleRedisCommand handles the redis command
func handleRedisCommand(client CommandHandlerClient, redisClient *redis.Client, ctx context.Context, mqttClient mqtt.Client, config *models.Config, params map[string]interface{}, requestID string) error {
	cmd, ok := params["cmd"].(string)
	if !ok {
		return fmt.Errorf("redis command not specified")
	}

	args, ok := params["args"].([]interface{})
	if !ok {
		args = []interface{}{}
	}

	var result interface{}
	var err error

	switch cmd {
	case "get":
		if len(args) != 1 {
			return fmt.Errorf("get requires exactly 1 argument")
		}
		result, err = redisClient.Get(ctx, args[0].(string)).Result()

	case "set":
		if len(args) != 2 {
			return fmt.Errorf("set requires exactly 2 arguments")
		}
		result, err = redisClient.Set(ctx, args[0].(string), args[1], 0).Result()

	case "hget":
		if len(args) != 2 {
			return fmt.Errorf("hget requires exactly 2 arguments")
		}
		result, err = redisClient.HGet(ctx, args[0].(string), args[1].(string)).Result()

	case "hset":
		if len(args) != 3 {
			return fmt.Errorf("hset requires exactly 3 arguments")
		}
		result, err = redisClient.HSet(ctx, args[0].(string), args[1].(string), args[2]).Result()

	case "hgetall":
		if len(args) != 1 {
			return fmt.Errorf("hgetall requires exactly 1 argument")
		}
		result, err = redisClient.HGetAll(ctx, args[0].(string)).Result()

	case "lpush":
		if len(args) < 2 {
			return fmt.Errorf("lpush requires at least 2 arguments")
		}
		key := args[0].(string)
		values := args[1:]
		result, err = redisClient.LPush(ctx, key, values...).Result()

	case "lpop":
		if len(args) != 1 {
			return fmt.Errorf("lpop requires exactly 1 argument")
		}
		result, err = redisClient.LPop(ctx, args[0].(string)).Result()

	case "publish":
		if len(args) != 2 {
			return fmt.Errorf("publish requires exactly 2 arguments")
		}
		channel, ok := args[0].(string)
		if !ok {
			return fmt.Errorf("publish channel must be a string")
		}
		message, ok := args[1].(string)
		if !ok {
			return fmt.Errorf("publish message must be a string")
		}
		result, err = redisClient.Publish(ctx, channel, message).Result()

	default:
		return fmt.Errorf("unsupported redis command: %s", cmd)
	}

	if err != nil {
		return fmt.Errorf("redis command failed: %v", err)
	}

	// Send response on the data topic
	response := map[string]interface{}{
		"type":       "redis",
		"command":    cmd,
		"result":     result,
		"request_id": requestID,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %v", err)
	}

	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)
	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish response: %v", token.Error())
	}

	return nil
}

// handleShellCommand handles the shell command asynchronously
func handleShellCommand(client CommandHandlerClient, mqttClient mqtt.Client, config *models.Config, params map[string]interface{}, requestID string, stream bool) error {
	cmdStr, ok := params["cmd"].(string)
	if !ok {
		return fmt.Errorf("shell command not specified")
	}

	// Execute command through shell to handle quotes, pipes, and other shell syntax
	if cmdStr == "" {
		return fmt.Errorf("empty command")
	}

	cmd := exec.Command("sh", "-c", cmdStr)

	// Create pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %v", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %v", err)
	}

	// Send immediate "process started" response with PID
	pid := cmd.Process.Pid
	client.SendCommandResponseWithPID(requestID, "process_started", "", pid)

	// Run the command execution asynchronously
	go func() {
		executeShellCommandAsync(cmd, stdout, stderr, mqttClient, config, requestID, stream)
	}()

	// Return immediately - the command has been started successfully
	return nil
}

// executeShellCommandAsync runs the shell command execution asynchronously
func executeShellCommandAsync(cmd *exec.Cmd, stdout, stderr io.ReadCloser, mqttClient mqtt.Client, config *models.Config, requestID string, stream bool) {
	topic := fmt.Sprintf("scooters/%s/data", config.Scooter.Identifier)

	// Function to send batched output as concatenated string
	sendBatchedOutput := func(outputType string, lines []string) error {
		if len(lines) == 0 {
			return nil
		}

		// Concatenate lines with newlines
		output := strings.Join(lines, "\n")

		response := map[string]interface{}{
			"type":        "shell",
			"output":      output,
			"stream":      stream,
			"done":        false,
			"output_type": outputType,
			"request_id":  requestID,
		}

		responseJSON, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("failed to marshal response: %v", err)
		}

		if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
			return fmt.Errorf("failed to publish response: %v", token.Error())
		}
		return nil
	}

	// Collect output
	var stdoutBuf, stderrBuf bytes.Buffer
	var wg sync.WaitGroup

	// Batching configuration
	const (
		batchSize         = 10                     // Send batch every 10 lines
		batchTimeout      = 500 * time.Millisecond // Or every 500ms, whichever comes first
		keepaliveInterval = 10 * time.Second       // Send keepalive every 10 seconds
	)

	// Keepalive mechanism - send empty frames periodically to maintain connection
	keepaliveDone := make(chan bool)
	if stream {
		go func() {
			ticker := time.NewTicker(keepaliveInterval)
			defer ticker.Stop()

			for {
				select {
				case <-keepaliveDone:
					return
				case <-ticker.C:
					// Send keepalive frame
					keepaliveResponse := map[string]interface{}{
						"type":       "shell",
						"output":     "",
						"stream":     true,
						"done":       false,
						"keepalive":  true,
						"request_id": requestID,
					}

					keepaliveJSON, err := json.Marshal(keepaliveResponse)
					if err != nil {
						log.Printf("Failed to marshal keepalive response: %v", err)
						continue
					}

					if token := mqttClient.Publish(topic, 1, false, keepaliveJSON); token.Wait() && token.Error() != nil {
						log.Printf("Failed to publish keepalive: %v", token.Error())
					}
				}
			}
		}()
	}

	// Read stdout with batching
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		var batch []string
		lastSent := time.Now()

		flushBatch := func() {
			if len(batch) > 0 && stream {
				sendBatchedOutput("stdout", batch)
				batch = nil
				lastSent = time.Now()
			}
		}

		for scanner.Scan() {
			text := scanner.Text()
			stdoutBuf.WriteString(text + "\n")

			if stream {
				batch = append(batch, text)

				// Send batch if we hit size limit or timeout
				if len(batch) >= batchSize || time.Since(lastSent) >= batchTimeout {
					flushBatch()
				}
			}
		}

		// Send remaining lines in batch
		flushBatch()
	}()

	// Read stderr with batching
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		var batch []string
		lastSent := time.Now()

		flushBatch := func() {
			if len(batch) > 0 && stream {
				sendBatchedOutput("stderr", batch)
				batch = nil
				lastSent = time.Now()
			}
		}

		for scanner.Scan() {
			text := scanner.Text()
			stderrBuf.WriteString(text + "\n")

			if stream {
				batch = append(batch, text)

				// Send batch if we hit size limit or timeout
				if len(batch) >= batchSize || time.Since(lastSent) >= batchTimeout {
					flushBatch()
				}
			}
		}

		// Send remaining lines in batch
		flushBatch()
	}()

	// Wait for all output to be read first
	wg.Wait()

	// Stop keepalive ticker if streaming
	if stream {
		keepaliveDone <- true
	}

	// Then wait for command to complete
	err := cmd.Wait()

	// Send final response
	response := map[string]interface{}{
		"type":       "shell",
		"stdout":     strings.TrimSpace(stdoutBuf.String()),
		"stderr":     strings.TrimSpace(stderrBuf.String()),
		"exit_code":  cmd.ProcessState.ExitCode(),
		"stream":     stream,
		"done":       true,
		"request_id": requestID,
		"pid":        cmd.Process.Pid,
	}

	if err != nil {
		response["error"] = err.Error()
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal final shell response: %v", err)
		return
	}

	if token := mqttClient.Publish(topic, 1, false, responseJSON); token.Wait() && token.Error() != nil {
		log.Printf("Failed to publish final shell response: %v", token.Error())
		return
	}
}

// handleNavigateCommand handles the navigate command
func handleNavigateCommand(redisClient *redis.Client, ctx context.Context, params map[string]interface{}, requestID string) error {
	lat, latOK := params["latitude"].(float64)
	lng, lngOK := params["longitude"].(float64)

	// Check if this is a clear navigation command (both latitude and longitude are nil or missing)
	if (!latOK && !lngOK) || (params["latitude"] == nil && params["longitude"] == nil) {
		// Clear all navigation fields
		fields := []string{"latitude", "longitude", "address", "timestamp", "destination"}
		if err := redisClient.HDel(ctx, "navigation", fields...).Err(); err != nil {
			return fmt.Errorf("failed to clear navigation destination in Redis: %v", err)
		}

		// Publish notification to the navigation channel
		if err := redisClient.Publish(ctx, "navigation", "cleared").Err(); err != nil {
			// Log the error but don't fail the command, as the HDEL succeeded
			log.Printf("Warning: Failed to publish navigation clear notification: %v", err)
		}

		log.Printf("Navigation target cleared")
		return nil
	}

	// Check if both coordinates are provided for setting a destination
	if !latOK || !lngOK {
		return fmt.Errorf("invalid or missing latitude/longitude parameters")
	}

	// Get optional address parameter
	address, _ := params["address"].(string)

	// Format coordinates as "latitude,longitude" string for legacy compatibility
	coords := fmt.Sprintf("%f,%f", lat, lng)

	// Get current timestamp in ISO8601 format
	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Set all navigation fields in Redis
	navFields := map[string]interface{}{
		"latitude":    fmt.Sprintf("%f", lat),
		"longitude":   fmt.Sprintf("%f", lng),
		"timestamp":   timestamp,
		"destination": coords, // Legacy field for backward compatibility
	}

	// Add address if provided
	if address != "" {
		navFields["address"] = address
	}

	if err := redisClient.HSet(ctx, "navigation", navFields).Err(); err != nil {
		return fmt.Errorf("failed to set navigation destination in Redis: %v", err)
	}

	// Publish notification to the navigation channel
	if err := redisClient.Publish(ctx, "navigation", "destination").Err(); err != nil {
		// Log the error but don't fail the command, as the HSET succeeded
		log.Printf("Warning: Failed to publish navigation destination update: %v", err)
	}

	if address != "" {
		log.Printf("Navigation target set to: %s (%s)", coords, address)
	} else {
		log.Printf("Navigation target set to: %s", coords)
	}
	return nil
}

// handleHibernateCommand handles the hibernate command
func handleHibernateCommand(redisClient *redis.Client, ctx context.Context) error {
	return redisClient.LPush(ctx, "scooter:power", "hibernate").Err()
}
