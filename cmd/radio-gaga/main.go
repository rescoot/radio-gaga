package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"radio-gaga/internal/client"
	"radio-gaga/internal/config"
)

// Version is set during the build process
var version string

func main() {
	if version != "" {
		log.Printf("Starting radio-gaga version %s", version)
	} else {
		log.Print("Starting radio-gaga development version")
	}

	// Parse command line flags
	flags := config.ParseFlags()

	// Load configuration
	cfg, err := config.LoadConfig(flags)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Create and start MQTT client
	mqttClient, err := client.NewScooterMQTTClient(cfg, version)
	if err != nil {
		log.Fatalf("Failed to create MQTT client: %v", err)
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
