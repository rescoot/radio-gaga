package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
	"radio-gaga/internal/utils"
)

// RunProbe attempts to establish an MQTT connection using cfg and subscribe to
// the per-scooter command topic. Returns nil if the connect + SUBACK round-trip
// succeeds within probeTimeout (and the connection is still up after
// stickinessWait), or an error otherwise. Stderr-loggable, exit-code-suitable.
//
// Used by the transactional replace machinery: a candidate config (and binary)
// is validated by spawning a child radio-gaga process with -probe, and the
// orchestrator commits or rolls back based on the child's exit code.
//
// The probe uses the **same** MQTT client-id as production. Sunshine's broker
// (mosquitto with the dynamic-security plugin) pins each per-scooter username
// to exactly one client-id; a probe-suffixed id gets rejected at CONNACK with
// "not Authorized". This means the orchestrator MUST ensure the live MQTT
// client is disconnected before spawning the probe — otherwise the probe and
// the live parent would fight over the broker session.
//
// The probe deliberately differs from a normal client startup:
//
//   - No will message, no retained-status publish, no Redis side effects, no
//     reconnect handler. The probe is read-only on the system.
//   - CleanSession=true so the broker doesn't accumulate session state for the
//     probe; the live parent's next reconnect (CleanSession=false) starts
//     fresh on its own terms.
//
// SUBACK on scooters/{vin}/commands proves TLS handshake, mosquitto auth, and
// per-scooter ACL all stack up correctly with the new config.
func RunProbe(cfg *models.Config, probeTimeout, stickinessWait time.Duration) error {
	if cfg.Scooter.Identifier == "" {
		return fmt.Errorf("probe requires scooter.identifier in config")
	}
	if cfg.Scooter.Token == "" {
		return fmt.Errorf("probe requires scooter.token in config")
	}
	if cfg.MQTT.BrokerURL == "" {
		return fmt.Errorf("probe requires mqtt.broker_url in config")
	}

	keepAlive := 30 * time.Second
	if cfg.MQTT.KeepAlive != "" {
		if d, err := time.ParseDuration(cfg.MQTT.KeepAlive); err == nil {
			keepAlive = d
		}
	}

	probeID := fmt.Sprintf("radio-gaga-%s", cfg.Scooter.Identifier)

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTT.BrokerURL).
		SetClientID(probeID).
		SetUsername(cfg.Scooter.Identifier).
		SetPassword(cfg.Scooter.Token).
		SetKeepAlive(keepAlive).
		SetAutoReconnect(false).
		SetConnectTimeout(probeTimeout).
		SetWriteTimeout(probeTimeout).
		SetPingTimeout(probeTimeout).
		SetCleanSession(true)

	if utils.IsTLSURL(cfg.MQTT.BrokerURL) {
		tlsCfg := new(tls.Config)
		switch {
		case cfg.MQTT.CACertEmbedded != "":
			pool := x509.NewCertPool()
			if ok := pool.AppendCertsFromPEM([]byte(cfg.MQTT.CACertEmbedded)); !ok {
				return fmt.Errorf("failed to parse embedded CA certificate")
			}
			tlsCfg.RootCAs = pool
		case cfg.MQTT.CACert != "":
			caBytes, err := os.ReadFile(cfg.MQTT.CACert)
			if err != nil {
				return fmt.Errorf("failed to read CA certificate %s: %v", cfg.MQTT.CACert, err)
			}
			pool := x509.NewCertPool()
			if ok := pool.AppendCertsFromPEM(caBytes); !ok {
				return fmt.Errorf("failed to parse CA certificate %s", cfg.MQTT.CACert)
			}
			tlsCfg.RootCAs = pool
		}
		opts.SetTLSConfig(tlsCfg)
	}

	// Reuse the package-private connect logic so we get the same NTP-sync /
	// insecure-TLS fallback behavior production has. If the live client
	// reaches the broker via the insecure fallback, the probe should too —
	// otherwise we'd reject a config production happily accepts.
	mqClient, err := createMQTTClient(cfg, opts)
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	defer mqClient.Disconnect(250)

	commandTopic := fmt.Sprintf("scooters/%s/commands", cfg.Scooter.Identifier)
	subToken := mqClient.Subscribe(commandTopic, 1, nil)
	if !subToken.WaitTimeout(probeTimeout) {
		return fmt.Errorf("subscribe to %s timed out (no SUBACK within %s)", commandTopic, probeTimeout)
	}
	if err := subToken.Error(); err != nil {
		return fmt.Errorf("subscribe to %s failed: %v", commandTopic, err)
	}

	if stickinessWait > 0 {
		time.Sleep(stickinessWait)
		if !mqClient.IsConnected() {
			return fmt.Errorf("connection dropped during %s stickiness wait", stickinessWait)
		}
	}

	log.Printf("probe: connected and subscribed to %s (client-id=%s)", commandTopic, probeID)
	return nil
}
