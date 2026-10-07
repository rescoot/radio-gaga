package client

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

func TestMQTTOptionsSeparateAuthenticationFromTopicIdentity(t *testing.T) {
	s, _ := newTestClient(t)
	if s.config.MQTTUsername() != s.config.Scooter.Identifier {
		t.Fatal("default authentication username differs from scooter identity")
	}
	s.config.MQTT.Username = "radio-gaga-866802022874384"
	opts := s.buildMQTTOptions()
	if opts.Username != s.config.MQTT.Username || opts.ClientID != "radio-gaga-"+s.config.Scooter.Identifier {
		t.Fatal("authentication override changed client identity or was ignored")
	}
}

func TestProbeUsesConfiguredAuthenticationUsername(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s, _ := newTestClient(t)
	s.config.Scooter.Token = "probe-secret"
	s.config.MQTT.BrokerURL = "tcp://" + listener.Addr().String()
	s.config.MQTT.Username = "radio-gaga-866802022874384"
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		packet, err := packets.ReadPacket(conn)
		if err == nil {
			connect, ok := packet.(*packets.ConnectPacket)
			if !ok || connect.Username != s.config.MQTT.Username {
				err = fmt.Errorf("unexpected authentication packet: %v", packet)
			}
		}
		if err == nil {
			err = packets.NewControlPacket(packets.Connack).Write(conn)
		}
		if err == nil {
			packet, err = packets.ReadPacket(conn)
		}
		if err == nil {
			subscribe, ok := packet.(*packets.SubscribePacket)
			if !ok || len(subscribe.Topics) != 1 || subscribe.Topics[0] != "scooters/"+s.config.Scooter.Identifier+"/commands" {
				err = fmt.Errorf("unexpected subscription: %v", packet)
			} else {
				ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
				ack.MessageID = subscribe.MessageID
				ack.ReturnCodes = []byte{1}
				err = ack.Write(conn)
			}
		}
		if err == nil {
			_, err = packets.ReadPacket(conn)
		}
		done <- err
	}()
	if err := RunProbe(s.config, time.Second, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("probe broker timed out")
	}
}
