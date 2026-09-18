package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
	"radio-gaga/internal/txn"
	"radio-gaga/internal/utils"
)

// The deferred half of the bootstrap flow.
//
// The one-shot flow in bootstrap.go needs the scooter to be online at the moment
// somebody installs it: it POSTs, gets a config, applies it, and exits. This mode
// instead writes nothing and waits. The scooter announces itself on MQTT with the
// code it was given, keeps announcing until the account owner accepts the claim,
// and only then receives a real config. That is what makes an offline install
// possible, and what makes the claim an actual decision by the owner rather than
// a side effect of knowing a code.
const (
	announceInterval      = 60 * time.Second
	announceConnectTO     = 30 * time.Second
	announceApplyTimeout  = 90 * time.Second
	announcePostCommitLag = 2 * time.Second
)

// bootstrapBrokerUsername derives the MQTT username from the short code.
//
// Both ends compute this from the code alone: the device hashes what it was
// given, and Sunshine hashes the code's digest it already stores. That is what
// lets a scooter that has never reached us authenticate, and what lets Sunshine
// revoke the credential later without holding the code's plaintext. It also keeps
// the code out of topics and out of broker logs — mosquitto logs the username,
// not the password.
func bootstrapBrokerUsername(code string) string {
	sum := sha256.Sum256([]byte(normalizeShortCode(code)))
	return hex.EncodeToString(sum[:])[:16]
}

// normalizeShortCode mirrors BootstrapToken.normalize_short_code on the server.
//
// Both ends have to agree exactly, because the code is simultaneously the broker
// password and the input to the username hash. A user who types a lowercase code,
// or reads an O as a 0, must still land on the credential the server created —
// a normalization difference would derive a username the broker has never heard
// of, and the failure would look like the code being wrong rather than the
// characters being folded differently.
//
// Crockford's decoding substitutions: O reads as 0, I and L read as 1, U reads as
// V. Non-alphanumerics (spaces, hyphens from a copied URL) are dropped.
func normalizeShortCode(code string) string {
	var kept strings.Builder
	for _, r := range strings.ToUpper(code) {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') {
			kept.WriteRune(r)
		}
	}
	// One pass, so a substitution can never feed another: none of the outputs are
	// inputs here.
	return strings.NewReplacer("O", "0", "I", "1", "L", "1", "U", "V").Replace(kept.String())
}

// announcePayload is what the device tells Sunshine about itself. One identifier
// is required; the nonce lets the server (and support) tell two devices holding
// one code apart.
func announcePayload(imei, mdbSerial, dbcSerial, softwareVersion, platform, name, nonce string) []byte {
	payload := map[string]string{
		"nonce":      nonce,
		"os_version": softwareVersion,
		"platform":   platform,
	}
	for key, value := range map[string]string{
		"imei": imei, "mdb_serial": mdbSerial, "dbc_serial": dbcSerial, "name": name,
	} {
		if value != "" {
			payload[key] = value
		}
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// txnReplaceEnvelope is the subset of the pushed command this mode needs.
type txnReplaceEnvelope struct {
	Command string `json:"command"`
	Params  struct {
		TxnID      string `json:"txn_id"`
		Kind       string `json:"kind"`
		ConfigYAML string `json:"config_yaml"`
		Restart    bool   `json:"restart"`
	} `json:"params"`
}

// runBootstrapAnnounce connects with the bootstrap credential, announces until a
// config arrives, applies it through the txn machinery, reports the outcome, and
// (on success) exits so systemd respawns into the real config.
//
// Returns nil only when a config was committed and the process should exit.
func runBootstrapAnnounce(cfg *models.Config, configPath, softwareVersion string) error {
	code := normalizeShortCode(cfg.Bootstrap.Code)
	if code == "" {
		return fmt.Errorf("bootstrap state without a code")
	}
	if configPath == "" {
		return fmt.Errorf("config path is required: the applied config is written there")
	}
	if cfg.MQTT.BrokerURL == "" {
		return fmt.Errorf("mqtt.broker_url is required to announce")
	}

	username := bootstrapBrokerUsername(code)
	nonce := randomNonce()
	platform := detectPlatform()

	imei := tryReadIMEI(bootstrapModemTimeout)
	mdbSerial, dbcSerial := tryReadSerials()
	if imei == "" && mdbSerial == "" {
		// Not fatal: the modem may still be enumerating, and the announce timer
		// will pick it up on a later tick.
		log.Printf("bootstrap-announce: no hardware identifier yet; will retry each tick")
	}
	log.Printf("bootstrap-announce: username=%s nonce=%s imei=%q mdb_sn=%q", username, nonce, imei, mdbSerial)

	configTopic := fmt.Sprintf("claim/%s/config", username)
	announceTopic := fmt.Sprintf("claim/%s/announce", username)
	resultTopic := fmt.Sprintf("claim/%s/result", username)

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTT.BrokerURL).
		SetClientID("radio-gaga-bootstrap-" + nonce).
		SetUsername(username).
		SetPassword(code).
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetConnectTimeout(announceConnectTO).
		SetConnectRetry(true).
		SetConnectRetryInterval(15 * time.Second).
		// No will: a pre-claim device has no scooter status topic, and claiming
		// one would be claiming an identity it does not have yet.
		SetCleanSession(true)

	if utils.IsTLSURL(cfg.MQTT.BrokerURL) {
		tlsConfig := &tls.Config{}
		var err error
		switch {
		case cfg.MQTT.CACertEmbedded != "":
			tlsConfig, err = utils.CreateInsecureTLSConfigWithEmbeddedCert(cfg.MQTT.CACertEmbedded)
		case cfg.MQTT.CACert != "":
			tlsConfig, err = utils.CreateInsecureTLSConfig(cfg.MQTT.CACert)
		}
		if err != nil {
			return fmt.Errorf("build TLS config: %w", err)
		}
		opts.SetTLSConfig(tlsConfig)
	}

	configs := make(chan []byte, 1)
	client := mqtt.NewClient(opts)

	if token := client.Connect(); token.WaitTimeout(announceConnectTO) && token.Error() != nil {
		return fmt.Errorf("connect as %s: %w", username, token.Error())
	}
	if !client.IsConnected() {
		return fmt.Errorf("connect as %s: timed out", username)
	}
	defer client.Disconnect(250)

	if token := client.Subscribe(configTopic, 1, func(_ mqtt.Client, message mqtt.Message) {
		payload := append([]byte(nil), message.Payload()...)
		select {
		case configs <- payload:
		default:
			// One config is all this mode can apply; a duplicate is either the
			// same retained document or a re-push while we are already working.
		}
	}); token.WaitTimeout(announceConnectTO) && token.Error() != nil {
		return fmt.Errorf("subscribe %s: %w", configTopic, token.Error())
	}
	log.Printf("bootstrap-announce: subscribed to %s, announcing on %s", configTopic, announceTopic)

	announce := func() {
		imei := imei
		if imei == "" {
			imei = tryReadIMEI(bootstrapModemTimeout)
		}
		serial := mdbSerial
		if serial == "" {
			serial, _ = tryReadSerials()
		}
		payload := announcePayload(imei, serial, dbcSerial, softwareVersion, platform, cfg.Scooter.Name, nonce)
		token := client.Publish(announceTopic, 1, false, payload)
		token.WaitTimeout(announceConnectTO)
		if token.Error() != nil {
			log.Printf("bootstrap-announce: publish failed: %v", token.Error())
		}
	}

	announce()
	ticker := time.NewTicker(announceInterval)
	defer ticker.Stop()

	for {
		select {
		case payload := <-configs:
			committed, err := applyPushedConfig(payload, configPath)
			status, reason := "success", ""
			if err != nil {
				status, reason = "rollback", err.Error()
				log.Printf("bootstrap-announce: apply failed: %v", err)
			} else if !committed {
				status, reason = "rollback", "transaction did not commit"
			}

			result := map[string]string{"status": status}
			if reason != "" {
				result["reason"] = reason
			}
			if body, err := json.Marshal(result); err == nil {
				token := client.Publish(resultTopic, 1, false, body)
				token.WaitTimeout(announceConnectTO)
			}

			if status != "success" {
				// Keep announcing: Sunshine can re-push, and the retained config
				// stays on the broker until the server clears it.
				log.Printf("bootstrap-announce: reported %s, continuing to announce", status)
				continue
			}

			// The committed config has no bootstrap stanza, so nothing needs
			// stripping: the pushed document simply omits it. Exit and let systemd
			// respawn into the real config.
			log.Printf("bootstrap-announce: config committed; restarting into it")
			client.Disconnect(250)
			time.Sleep(announcePostCommitLag)
			return nil

		case <-ticker.C:
			announce()
		}
	}
}

// applyPushedConfig runs the pushed txn:replace through the same crash-safe
// machinery every other config change uses: stage, probe with the new config,
// commit or roll back.
func applyPushedConfig(payload []byte, configPath string) (bool, error) {
	var envelope txnReplaceEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false, fmt.Errorf("parse pushed command: %w", err)
	}
	if envelope.Command != "txn:replace" {
		return false, fmt.Errorf("unexpected command %q", envelope.Command)
	}
	if envelope.Params.ConfigYAML == "" {
		return false, fmt.Errorf("pushed command carried no config")
	}
	if kind := envelope.Params.Kind; kind != "" && kind != string(txn.KindConfig) {
		return false, fmt.Errorf("unsupported kind %q", kind)
	}

	exePath, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve executable: %w", err)
	}

	manager := &txn.Manager{
		LiveConfigPath: configPath,
		LiveBinaryPath: exePath,
		PendingPath:    filepath.Join(filepath.Dir(configPath), ".txn-pending.json"),
		Logger:         log.Default(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), announceApplyTimeout)
	defer cancel()

	txnID := envelope.Params.TxnID
	if txnID == "" {
		txnID = fmt.Sprintf("bootstrap-announce-%d", time.Now().Unix())
	}

	committed, runErr := manager.Run(ctx, txnID, txn.KindConfig,
		txn.Candidate{Config: []byte(envelope.Params.ConfigYAML)},
		txn.SubprocessProbe(log.Default().Writer()))
	if runErr != nil {
		return committed, runErr
	}
	return committed, nil
}

func randomNonce() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
