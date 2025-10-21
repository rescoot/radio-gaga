package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"radio-gaga/internal/client"
	"radio-gaga/internal/config"
	"radio-gaga/internal/handlers"
)

// Version is set during the build process
var version string

func main() {
	// Create logger
	if os.Getenv("INVOCATION_ID") != "" {
		log.SetOutput(os.Stdout)
		log.SetFlags(0)
		log.SetPrefix("")
	} else {
		log.SetOutput(os.Stdout)
		log.SetFlags(log.LstdFlags | log.Lmsgprefix)
		log.SetPrefix("radio-gaga: ")
	}

	if version != "" {
		log.Printf("Starting radio-gaga version %s", version)
	} else {
		log.Print("Starting radio-gaga development version")
	}

	// Parse command line flags
	flags := config.ParseFlags()

	// Load configuration
	cfg, configPath, err := config.LoadConfig(flags)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Create and start MQTT client
	mqttClient, err := client.NewScooterMQTTClient(cfg, configPath, version)
	if err != nil {
		log.Fatalf("Failed to create MQTT client: %v", err)
	}

	// Reconfigure unu-uplink if enabled (before starting MQTT client)
	if cfg.UnuUplink.Enabled {
		ctx := context.Background()
		if err := handlers.ReconfigureUnuUplink(ctx, mqttClient.GetRedisClient(), cfg); err != nil {
			log.Printf("Warning: Failed to reconfigure unu-uplink: %v", err)
		}
	}

	if err := mqttClient.Start(); err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}

	// Handle interrupts for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Stop client on shutdown
	mqttClient.Stop()
}
