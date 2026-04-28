package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"radio-gaga/internal/client"
	"radio-gaga/internal/config"
	"radio-gaga/internal/handlers"
	"radio-gaga/internal/handlers/commands"
	"radio-gaga/internal/journalupload"
	"radio-gaga/internal/txn"
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

	// Bootstrap mode: per-user-token install flow. Reads hardware IDs, calls
	// Sunshine's bootstrap endpoint, hands the returned config YAML to the
	// txn machinery, exits. Used as a one-shot during install — outside the
	// systemd lifecycle. Skips LoadConfig entirely; the bootstrap handler
	// only needs the -api-base-url and -config (write target) flags.
	if flags.Bootstrap {
		if err := runBootstrap(flags.BootstrapToken, flags.APIBaseURL, flags.ConfigPath, version); err != nil {
			log.Printf("bootstrap failed: %v", err)
			os.Exit(probeFailExitCode)
		}
		log.Printf("bootstrap: complete")
		os.Exit(0)
	}

	// Boot-time txn recovery: runs BEFORE LoadConfig so a rolled-back config
	// is what gets parsed. Skipped for probe mode — probe runs as a child of
	// an orchestrator that already did its own recovery; touching txn state
	// from inside the probe would interfere with the in-flight transaction.
	if !flags.Probe {
		bootCfgPath := flags.ConfigPath
		if bootCfgPath == "" {
			bootCfgPath = "radio-gaga.yml"
		}
		txnManager := newTxnManager(bootCfgPath)
		if err := txnManager.RecoverOnBoot(); err != nil {
			log.Printf("Warning: txn recovery failed: %v (continuing with current on-disk state)", err)
		}
	}

	// Load configuration (post-recovery in normal mode).
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
	// on the per-scooter command topic, and exit. The orchestrator commits the
	// candidate only if this child returns 0.
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

// newTxnManager builds a txn.Manager rooted at the live config path. The
// .staging and .lkg files live alongside the config (same filesystem so
// os.Rename is atomic); the pending marker lives in the same directory.
func newTxnManager(configPath string) *txn.Manager {
	dir := filepath.Dir(configPath)
	return &txn.Manager{
		LiveConfigPath: configPath,
		PendingPath:    filepath.Join(dir, ".txn-pending.json"),
		Logger:         log.Default(),
	}
}
