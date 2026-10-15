package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"radio-gaga/internal/models"
	"radio-gaga/internal/modeminfo"
)

type bootstrapBroker struct {
	command string
	path    string
	log     *os.File
	process *exec.Cmd
	config  models.MQTTConfig
}

func newBootstrapBroker(t *testing.T) *bootstrapBroker {
	t.Helper()
	command, err := exec.LookPath("mosquitto")
	if err != nil {
		t.Skip("mosquitto is required for the local TLS MQTT integration test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	cert, ca := bootstrapTestCertificate(t, "localhost", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string][]byte{
		"cert.pem": []byte(ca),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf("listener %d 127.0.0.1\nallow_anonymous true\npersistence false\ncertfile %s\nkeyfile %s\n", port, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if os.Geteuid() == 0 {
		config += "user root\n"
	}
	path := filepath.Join(dir, "mosquitto.conf")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "mosquitto.log"))
	if err != nil {
		t.Fatal(err)
	}
	broker := &bootstrapBroker{
		command: command, path: path, log: logFile,
		config: models.MQTTConfig{BrokerURL: fmt.Sprintf("ssl://localhost:%d", port), CACertEmbedded: ca},
	}
	t.Cleanup(func() {
		broker.stop()
		logFile.Close()
		if t.Failed() {
			body, _ := os.ReadFile(logFile.Name())
			t.Logf("MQTT broker log: %s", body)
		}
	})
	broker.start(t)
	return broker
}

func (b *bootstrapBroker) start(t *testing.T) {
	t.Helper()
	info, err := b.log.Stat()
	if err != nil {
		t.Fatal(err)
	}
	b.process = exec.Command(b.command, "-c", b.path)
	b.process.Stdout, b.process.Stderr = b.log, b.log
	if err := b.process.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		body, err := os.ReadFile(b.log.Name())
		if err == nil && int64(len(body)) >= info.Size() && strings.Contains(string(body[info.Size():]), " running\n") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, _ := os.ReadFile(b.log.Name())
	t.Fatalf("local MQTT broker did not start: %s", body)
}

func (b *bootstrapBroker) stop() {
	if b.process != nil {
		b.process.Process.Kill()
		b.process.Wait()
		b.process = nil
	}
}

type bootstrapEvent struct {
	topic string
	body  []byte
}

func bootstrapObserver(t *testing.T, broker *bootstrapBroker) (mqtt.Client, <-chan bootstrapEvent) {
	t.Helper()
	tlsConfig, err := bootstrapTLSConfig(broker.config)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan bootstrapEvent, 128)
	opts := mqtt.NewClientOptions().AddBroker(broker.config.BrokerURL).
		SetClientID("test-observer-" + randomNonce()).SetTLSConfig(tlsConfig).SetAutoReconnect(true)
	ready := make(chan error, 8)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		token := client.Subscribe("claim/#", 1, func(_ mqtt.Client, message mqtt.Message) {
			events <- bootstrapEvent{topic: message.Topic(), body: append([]byte(nil), message.Payload()...)}
		})
		if !token.WaitTimeout(5 * time.Second) {
			ready <- fmt.Errorf("observer subscription timed out")
			return
		}
		ready <- token.Error()
	})
	client := mqtt.NewClient(opts)
	t.Cleanup(func() { client.Disconnect(100) })
	awaitMQTT(t, client.Connect())
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not subscribe")
	}
	return client, events
}

func awaitMQTT(t *testing.T, token mqtt.Token) {
	t.Helper()
	if !token.WaitTimeout(5 * time.Second) {
		t.Fatal("MQTT operation timed out")
	}
	if err := token.Error(); err != nil {
		t.Fatal(err)
	}
}

func awaitBootstrapEvent(t *testing.T, events <-chan bootstrapEvent, topic, id string) map[string]string {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.topic != topic {
				continue
			}
			var body map[string]string
			if err := json.Unmarshal(event.body, &body); err != nil {
				t.Fatal(err)
			}
			if body["installation_id"] == id {
				return body
			}
		case <-timer.C:
			t.Fatalf("no event on %s for %s", topic, id)
		}
	}
}

func startBootstrapTestSession(t *testing.T, broker *bootstrapBroker, id string) (context.CancelFunc, <-chan error, <-chan txnReplaceEnvelope) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	applied := make(chan txnReplaceEnvelope, 8)
	cfg := &models.Config{
		Bootstrap: models.BootstrapConfig{Code: "FGHX7A", InstallationID: id},
		Scooter:   models.ScooterConfig{Name: "My scooter"}, MQTT: broker.config,
	}
	identity := modeminfo.Identity{IMEI: "123456789012345", ICCID: "8949000000000000001", IMSI: "901280000000001"}
	if id == testInstallationB {
		identity = modeminfo.Identity{IMEI: "234567890123456", ICCID: "8949000000000000002", IMSI: "901280000000002"}
	}
	path := filepath.Join(t.TempDir(), "config.yml")
	go func() {
		done <- runBootstrapSession(ctx, cfg, path, "test", bootstrapDependencies{
			readIMEI:      func() string { return identity.IMEI },
			readSerials:   func() (string, string) { return "mdb", "dbc" },
			fetchIdentity: func(context.Context) (modeminfo.Identity, error) { return identity, nil },
			applyConfig: func(body []byte, _ string) (bool, error) {
				var envelope txnReplaceEnvelope
				if err := json.Unmarshal(body, &envelope); err != nil {
					return false, err
				}
				applied <- envelope
				return envelope.Params.ConfigYAML != "rollback", nil
			},
		})
	}()
	t.Cleanup(cancel)
	return cancel, done, applied
}

func awaitBootstrapApply(t *testing.T, applied <-chan txnReplaceEnvelope, txnID string) {
	t.Helper()
	select {
	case delivery := <-applied:
		if delivery.Params.TxnID != txnID {
			t.Fatalf("applied %s, wanted %s", delivery.Params.TxnID, txnID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configuration was not applied")
	}
}

func awaitBootstrapExit(t *testing.T, done <-chan error, success bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err == nil) != success {
			t.Fatalf("session exit: %v, want success=%v", err, success)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap session did not exit")
	}
}

func TestBootstrapMQTTSharedCredentialIsolation(t *testing.T) {
	broker := newBootstrapBroker(t)
	server, events := bootstrapObserver(t, broker)
	username := bootstrapBrokerUsername("FGHX7A")
	bindingA := bootstrapBinding{installationID: testInstallationA}
	bindingB := bootstrapBinding{installationID: testInstallationB}
	announce, configA, resultA := bindingA.topics(username)
	_, configB, resultB := bindingB.topics(username)
	_, doneA, appliedA := startBootstrapTestSession(t, broker, testInstallationA)
	a := awaitBootstrapEvent(t, events, announce, testInstallationA)
	_, doneB, appliedB := startBootstrapTestSession(t, broker, testInstallationB)
	b := awaitBootstrapEvent(t, events, announce, testInstallationB)
	if a["nonce"] == b["nonce"] || a["imei"] == b["imei"] || a["name"] != "My scooter" || b["name"] != "My scooter" {
		t.Fatal("announcements did not distinguish same-name scooters")
	}
	for _, body := range [][]byte{
		boundDelivery(testInstallationB, b["nonce"], "wrong-installation", "config"),
		boundDelivery(testInstallationA, "stale-process", "stale", "config"),
		boundDelivery("", "", "unbound", "config"),
	} {
		awaitMQTT(t, server.Publish(configA, 1, false, body))
	}
	awaitMQTT(t, server.Publish("claim/"+username+"/config", 1, false, boundDelivery(testInstallationA, a["nonce"], "shared-topic", "config")))
	awaitMQTT(t, server.Publish(configA, 1, false, boundDelivery(testInstallationA, a["nonce"], "rollback-a", "rollback")))
	awaitBootstrapApply(t, appliedA, "rollback-a")
	rollback := awaitBootstrapEvent(t, events, resultA, testInstallationA)
	if rollback["status"] != "rollback" || rollback["nonce"] != a["nonce"] {
		t.Fatalf("unbound rollback: %v", rollback)
	}
	awaitMQTT(t, server.Publish(configA, 1, false, boundDelivery(testInstallationA, a["nonce"], "commit-a", "config-a")))
	awaitBootstrapApply(t, appliedA, "commit-a")
	result := awaitBootstrapEvent(t, events, resultA, testInstallationA)
	if result["status"] != "success" || result["nonce"] != a["nonce"] {
		t.Fatalf("unbound success: %v", result)
	}
	awaitMQTT(t, server.Publish(configB, 1, false, boundDelivery(testInstallationB, b["nonce"], "commit-b", "config-b")))
	awaitBootstrapApply(t, appliedB, "commit-b")
	result = awaitBootstrapEvent(t, events, resultB, testInstallationB)
	if result["status"] != "success" || result["nonce"] != b["nonce"] {
		t.Fatalf("unbound success: %v", result)
	}
	awaitBootstrapExit(t, doneA, true)
	awaitBootstrapExit(t, doneB, true)
}

func TestBootstrapMQTTRestartRejectsRetainedConfig(t *testing.T) {
	broker := newBootstrapBroker(t)
	server, events := bootstrapObserver(t, broker)
	binding := bootstrapBinding{installationID: testInstallationA}
	announce, config, _ := binding.topics(bootstrapBrokerUsername("FGHX7A"))
	cancel, done, _ := startBootstrapTestSession(t, broker, testInstallationA)
	first := awaitBootstrapEvent(t, events, announce, testInstallationA)
	cancel()
	awaitBootstrapExit(t, done, false)
	awaitMQTT(t, server.Publish(config, 1, true, boundDelivery(testInstallationA, first["nonce"], "stale-retained", "config")))
	_, done, applied := startBootstrapTestSession(t, broker, testInstallationA)
	second := awaitBootstrapEvent(t, events, announce, testInstallationA)
	if first["nonce"] == second["nonce"] {
		t.Fatal("a restarted process must have a new nonce")
	}
	awaitMQTT(t, server.Publish(config, 1, false, boundDelivery(testInstallationA, second["nonce"], "fresh", "config")))
	awaitBootstrapApply(t, applied, "fresh")
	awaitBootstrapExit(t, done, true)
}

func TestBootstrapMQTTReconnectResubscribes(t *testing.T) {
	broker := newBootstrapBroker(t)
	_, events := bootstrapObserver(t, broker)
	binding := bootstrapBinding{installationID: testInstallationA}
	announce, config, _ := binding.topics(bootstrapBrokerUsername("FGHX7A"))
	_, done, applied := startBootstrapTestSession(t, broker, testInstallationA)
	first := awaitBootstrapEvent(t, events, announce, testInstallationA)
	broker.stop()
	broker.start(t)
	server, freshEvents := bootstrapObserver(t, broker)
	second := awaitBootstrapEvent(t, freshEvents, announce, testInstallationA)
	if first["nonce"] != second["nonce"] {
		t.Fatal("a broker reconnect must preserve the process binding")
	}
	// A retained response also reaches a client whose subscribe is still completing.
	awaitMQTT(t, server.Publish(config, 1, true, boundDelivery(testInstallationA, second["nonce"], "reconnected", "config")))
	awaitBootstrapApply(t, applied, "reconnected")
	awaitBootstrapExit(t, done, true)
}
