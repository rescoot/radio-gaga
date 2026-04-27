package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"radio-gaga/internal/client"
	"radio-gaga/internal/config"
	"radio-gaga/internal/handlers"
	"radio-gaga/internal/handlers/commands"
	"radio-gaga/internal/journalupload"
)

const (
	probeConnectTimeout  = 30 * time.Second
	probeStickinessWait  = 2 * time.Second
	probeFailExitCode    = 2
)

// Version is set during the build process
var version string

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")

	// Parse command line flags (this calls flag.Parse())
	flags := config.ParseFlags()

	if *showVersion {
		if version != "" {
			fmt.Println(version)
		} else {
			fmt.Println("development")
		}
		os.Exit(0)
	}

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

	// Load configuration
	cfg, configPath, err := config.LoadConfig(flags)
	if err != nil {
		if flags.Probe {
			// Surface the load error specifically — the orchestrator needs to
			// see why the candidate config didn't even parse.
			log.Printf("probe: failed to load config: %v", err)
			os.Exit(probeFailExitCode)
		}
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Probe mode: connect to MQTT once with the loaded config, verify a SUBACK
	// on the per-scooter command topic, and exit. Used by the transactional
	// replace machinery — the orchestrator commits the candidate only if this
	// child returns 0.
	if flags.Probe {
		if err := client.RunProbe(cfg, probeConnectTimeout, probeStickinessWait); err != nil {
			log.Printf("probe: %v", err)
			os.Exit(probeFailExitCode)
		}
		log.Printf("probe: success")
		os.Exit(0)
	}

	// Route journal-upload session/cursor files through the auto-detected
	// state directory. cfg.StateDir is always populated by config.LoadConfig.
	commands.JournalUploadStateDir = cfg.StateDir

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

	// Re-arm journal streaming if a session file is present from a prior run
	// (e.g. after a reboot). Idempotent: if no session, this is a no-op.
	go func() {
		state := &journalupload.State{Dir: commands.JournalUploadStateDir}
		if _, ok, _ := state.ReadSession(); !ok {
			return
		}
		log.Printf("journalupload: re-arming streaming from persisted session")
		commands.StartManager(state)
	}()

	// Handle interrupts for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Stop client on shutdown
	mqttClient.Stop()
}
