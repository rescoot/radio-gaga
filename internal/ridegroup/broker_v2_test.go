package ridegroup

import (
	"context"
	"encoding/json"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"io"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestV2ActualBrokerDiscoveryJournalActivityPositionAndNoFix(t *testing.T) {
	path, err := exec.LookPath("mosquitto")
	if err != nil {
		t.Fatal("mosquitto required for live adapter validation")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	broker := exec.Command(path, "-p", strconv.Itoa(port))
	broker.Stdout, broker.Stderr = io.Discard, io.Discard
	if err = broker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Process.Kill(); _ = broker.Wait() })
	address := fmt.Sprintf("tcp://127.0.0.1:%d", port)
	observer := connectTestMQTT(t, address, "v2-observer")
	defer observer.Disconnect(100)
	messages := make(chan mqtt.Message, 32)
	token := observer.Subscribe("scooters/vehicle-1/ride_group/up", 1, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case messages <- m:
		default:
		}
	})
	if !token.WaitTimeout(time.Second) || token.Error() != nil {
		t.Fatal("observer subscription")
	}
	device := connectTestMQTT(t, address, "v2-device")
	defer device.Disconnect(100)
	r := newTestRedis(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controls := r.client.Subscribe(ctx, controlChannel)
	defer controls.Close()
	if _, err := controls.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := NewAdapter(ctx, Config{Enabled: true, Identifier: "vehicle-1", ProtocolVersion: 2}, r.client)
	if err != nil {
		t.Fatal(err)
	}
	a.Start()
	defer a.Stop()
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.settingsReady })
	a.OnConnected(device)
	receiveType := func(kind string) []byte {
		t.Helper()
		for {
			m := receiveBrokerMessage(t, messages)
			var h struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(m.Payload(), &h) == nil && h.Type == kind {
				return m.Payload()
			}
		}
	}
	hello, err := ParseHelloV2(receiveType(v2HelloType))
	if err != nil {
		t.Fatal(err)
	}
	timeout, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	var probe LocalStopProbeV2
	for {
		m, err := controls.ReceiveMessage(timeout)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := ParseLocalStopProbeV2([]byte(m.Payload)); err == nil {
			probe = v
			break
		}
	}
	publishJSON(t, r.client, activityChannel, LocalStopV2{Version: 2, Type: v2LocalStopType, JournalRevision: 1, Phase: "idle", Challenge: probe.Challenge})
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.journalReady })
	activity := ActivityV2{Version: 2, Type: v2ActivityType, BootNonce: testBoot, Sequence: 1, Active: true, Challenge: testChallenge}
	publishJSON(t, r.client, activityChannel, activity)
	if _, err := ParseWireActivityV2(receiveType(v2ActivityType)); err != nil {
		t.Fatal(err)
	}
	grant := v2Grant(t)
	grant.ConnectionNonce = hello.ConnectionNonce
	grant.ServerTimeMS = time.Now().UnixMilli()
	publishMQTTJSON(t, observer, "scooters/vehicle-1/ride_group/down", 1, grant)
	for {
		m, err := controls.ReceiveMessage(timeout)
		if err != nil {
			t.Fatal("discovery grant never reached Qt", err)
		}
		if v, err := ParseControlV2([]byte(m.Payload)); err == nil && v.Action == "grant" {
			break
		}
	}
	waitFor(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.grantV2 != nil })
	time.Sleep(time.Second)
	activity.Sequence = 2
	activity.SessionID = strPtr(testSession)
	activity.MemberID = strPtr(testMember)
	activity.Challenge = "00000000-0000-4000-8000-000000000023"
	publishJSON(t, r.client, activityChannel, activity)
	receiveType(v2ActivityType)
	captured := time.Now().UTC()
	publishJSON(t, r.client, gpsChannel, testGPSFix("0.0", "0.0", captured, 3))
	position, err := ParsePositionV2(receiveType(v2PositionType))
	if err != nil {
		t.Fatal(err)
	}
	if position.CapturedAtMS != captured.UnixMilli() || position.Latitude != 0 || position.Longitude != 0 || position.SpeedMPS != nil {
		t.Fatal("producer sample misrepresented")
	}
	invalid := testGPSFix("0.0", "0.0", captured.Add(time.Millisecond), 3)
	invalid.Active = false
	publishJSON(t, r.client, gpsChannel, invalid)
	status, err := ParseSourceStatusV2(receiveType(v2SourceStatusType))
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "no_fix" || status.CapturedAtMS != nil || status.Sequence <= position.Sequence {
		t.Fatal("no-fix did not close source epoch")
	}
}
