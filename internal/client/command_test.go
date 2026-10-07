package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
	"radio-gaga/internal/models"
)

func TestCommandQueueDoesNotBlockCallback(t *testing.T) {
	s, _ := newTestClient(t)
	s.commandQueue = make(chan queuedCommand, 1)
	c := &fakeMQTTClient{}
	message := &fakeMessage{topic: "commands"}
	done := make(chan struct{})
	go func() {
		s.handleCommand(c, message)
		s.handleCommand(c, message)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MQTT callback blocked on a full command queue")
	}
	if len(s.commandQueue) != 1 {
		t.Fatal("queue exceeded its capacity")
	}
	if c.publishCalls != 0 {
		t.Fatal("MQTT callback executed a command")
	}
}

func TestCancelledCommandCallbackDoesNotEnqueue(t *testing.T) {
	s, _ := newTestClient(t)
	s.ctx, s.cancel = context.WithCancel(s.ctx)
	s.cancel()
	s.commandQueue = make(chan queuedCommand, 1)
	s.handleCommand(&fakeMQTTClient{}, &fakeMessage{})
	if len(s.commandQueue) != 0 {
		t.Fatal("cancelled client accepted a command")
	}
}

// The broker replays its full inflight window before responding to SUBSCRIBE.
// Command responses require PUBACKs from the same receive loop.
func TestPersistentCommandReplayKeepsMQTTReceiveLoopRunning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	brokerDone := make(chan error, 1)
	go func() { brokerDone <- serveCommandReplay(listener) }()
	s, _ := newTestClient(t)
	s.ctx, s.cancel = context.WithCancel(s.ctx)
	s.commandQueue = make(chan queuedCommand, commandQueueCapacity)
	s.config.MQTT.BrokerURL = "tcp://" + listener.Addr().String()
	s.startCommandWorker()
	defer func() { s.cancel(); s.wg.Wait() }()
	options := s.buildMQTTOptions().SetAutoReconnect(false)
	if !options.AutoAckDisabled {
		t.Fatal("command acknowledgements must be controlled by worker")
	}
	client, err := createMQTTClient(s.config, options, "scooters/test-scooter/commands", s.handleCommand, s.setActiveMQTTClient)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(10)
	select {
	case err := <-brokerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued commands blocked subscription or response acknowledgements")
	}
}

func serveCommandReplay(listener net.Listener) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err = packets.ReadPacket(conn); err != nil {
		return err
	}
	ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	if err = ack.Write(conn); err != nil {
		return err
	}
	for i := 1; i <= 20; i++ {
		p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
		p.TopicName = "scooters/test-scooter/commands"
		p.Qos = 1
		p.Dup = true
		p.MessageID = uint16(i)
		p.Payload = []byte(fmt.Sprintf(`{"command":"ping","request_id":"request-%d"}`, i))
		if err = p.Write(conn); err != nil {
			return err
		}
	}
	responses, acknowledgements := 0, 0
	subscribed := false
	for responses < 20 || acknowledgements < 20 || !subscribed {
		packet, err := packets.ReadPacket(conn)
		if err != nil {
			return err
		}
		switch p := packet.(type) {
		case *packets.PublishPacket:
			if p.TopicName == "scooters/test-scooter/acks" {
				var response models.CommandResponse
				if err := json.Unmarshal(p.Payload, &response); err != nil {
					return err
				}
				responses++
				if response.RequestID != fmt.Sprintf("request-%d", responses) || response.Status != "success" {
					return fmt.Errorf("out-of-order or failed response: %+v", response)
				}
			}
			if p.Qos == 1 {
				a := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				a.MessageID = p.MessageID
				if err = a.Write(conn); err != nil {
					return err
				}
			}
		case *packets.SubscribePacket:
			subscribed = true
			a := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			a.MessageID = p.MessageID
			a.ReturnCodes = []byte{1}
			if err = a.Write(conn); err != nil {
				return err
			}
		case *packets.PubackPacket:
			acknowledgements++
			if p.MessageID != uint16(acknowledgements) {
				return fmt.Errorf("out-of-order command acknowledgement: %d", p.MessageID)
			}
		}
	}
	return nil
}

type acknowledgedCommandMessage struct {
	fakeMessage
	acknowledged chan struct{}
}

func (m *acknowledgedCommandMessage) Ack() { close(m.acknowledged) }

func TestCommandWorkerLeavesStaleAndCancelledMessagesUnacknowledged(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			s, _ := newTestClient(t)
			s.ctx, s.cancel = context.WithCancel(s.ctx)
			s.commandQueue = make(chan queuedCommand, commandQueueCapacity)
			oldClient, activeClient := &fakeMQTTClient{}, &fakeMQTTClient{}
			s.setActiveMQTTClient(activeClient)
			stale := &acknowledgedCommandMessage{acknowledged: make(chan struct{})}
			current := &acknowledgedCommandMessage{
				fakeMessage:  fakeMessage{payload: []byte(`{"command":"ping","request_id":"current"}`)},
				acknowledged: make(chan struct{}),
			}
			s.handleCommand(oldClient, stale)
			s.handleCommand(activeClient, current)
			if cancelled {
				s.cancel()
			}
			s.startCommandWorker()
			if !cancelled {
				select {
				case <-current.acknowledged:
				case <-time.After(time.Second):
					t.Fatal("active command was not processed")
				}
			}
			s.cancel()
			s.wg.Wait()
			select {
			case <-stale.acknowledged:
				t.Fatal("stale command was acknowledged")
			default:
			}
			if oldClient.publishCalls != 0 {
				t.Fatal("stale command was executed")
			}
			if cancelled && activeClient.publishCalls != 0 {
				t.Fatal("cancelled command was executed")
			}
		})
	}
}

var _ mqtt.Message = (*acknowledgedCommandMessage)(nil)
