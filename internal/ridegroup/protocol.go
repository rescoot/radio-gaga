package ridegroup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

const MaxEnvelopeBytes = 1024

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type Activity struct {
	Version   int     `json:"v"`
	Type      string  `json:"type"`
	BootNonce string  `json:"boot_nonce"`
	Sequence  int64   `json:"seq"`
	Active    bool    `json:"active"`
	LocalStop bool    `json:"local_stop"`
	SessionID *string `json:"session_id"`
	MemberID  *string `json:"member_id"`
	Challenge string  `json:"challenge"`
}

type WireActivity struct {
	Activity
	ConnectionNonce string `json:"connection_nonce"`
}

type PeerDescriptor struct {
	PeerID       string `json:"peer_id"`
	Label        string `json:"label"`
	PaletteIndex int    `json:"palette_index"`
}

type Control struct {
	Version         int              `json:"v"`
	Type            string           `json:"type"`
	ConnectionNonce string           `json:"connection_nonce"`
	Action          string           `json:"action"`
	SessionID       *string          `json:"session_id"`
	MemberID        *string          `json:"member_id"`
	Revision        int64            `json:"revision"`
	TransportEpoch  int64            `json:"transport_epoch"`
	StreamID        *string          `json:"stream_id"`
	Challenge       string           `json:"challenge"`
	ServerTimeMS    int64            `json:"server_time_ms"`
	LeaseTTLMS      int64            `json:"lease_ttl_ms"`
	CadenceMS       *int64           `json:"cadence_ms"`
	Peers           []PeerDescriptor `json:"peers"`
}

type Position struct {
	Version         int      `json:"v"`
	Type            string   `json:"type"`
	SessionID       string   `json:"session_id"`
	Revision        int64    `json:"revision"`
	TransportEpoch  int64    `json:"transport_epoch"`
	StreamID        string   `json:"stream_id"`
	Sequence        int64    `json:"seq"`
	CapturedAtMS    int64    `json:"captured_at_ms"`
	TimestampSource string   `json:"timestamp_source"`
	Latitude        float64  `json:"lat"`
	Longitude       float64  `json:"lon"`
	AccuracyM       *float64 `json:"accuracy_m"`
	HeadingDeg      *float64 `json:"heading_deg"`
	SpeedMPS        *float64 `json:"speed_mps"`
	MotionSource    string   `json:"motion_source"`
}

type PeerPosition struct {
	Version            int      `json:"v"`
	Type               string   `json:"type"`
	SessionID          string   `json:"session_id"`
	Revision           int64    `json:"revision"`
	TransportEpoch     int64    `json:"transport_epoch"`
	StreamID           string   `json:"stream_id"`
	PeerStreamID       string   `json:"peer_stream_id"`
	PeerTransportEpoch int64    `json:"peer_transport_epoch"`
	Sequence           int64    `json:"seq"`
	PeerID             string   `json:"peer_id"`
	Label              string   `json:"label"`
	PaletteIndex       int      `json:"palette_index"`
	CapturedAtMS       int64    `json:"captured_at_ms"`
	TimestampSource    string   `json:"timestamp_source"`
	Latitude           float64  `json:"lat"`
	Longitude          float64  `json:"lon"`
	AccuracyM          *float64 `json:"accuracy_m"`
	HeadingDeg         *float64 `json:"heading_deg"`
	SpeedMPS           *float64 `json:"speed_mps"`
	MotionSource       string   `json:"motion_source"`
	ReceivedAtMS       int64    `json:"received_at_ms"`
	ServerTimeMS       int64    `json:"server_time_ms"`
	ValidForMS         int64    `json:"valid_for_ms"`
}

type PeerStatus struct {
	Version            int    `json:"v"`
	Type               string `json:"type"`
	SessionID          string `json:"session_id"`
	Revision           int64  `json:"revision"`
	TransportEpoch     int64  `json:"transport_epoch"`
	StreamID           string `json:"stream_id"`
	PeerID             string `json:"peer_id"`
	PeerStreamID       string `json:"peer_stream_id"`
	PeerTransportEpoch int64  `json:"peer_transport_epoch"`
	Status             string `json:"status"`
}

func decodeExact(data []byte, target any, keys ...string) error {
	if len(data) == 0 || len(data) > MaxEnvelopeBytes || !utf8.Valid(data) || rejectDuplicateKeys(data) != nil {
		return fmt.Errorf("invalid ride-group envelope size or encoding")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil || len(object) != len(keys) {
		return fmt.Errorf("invalid ride-group envelope")
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return fmt.Errorf("invalid ride-group envelope")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid ride-group envelope")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid ride-group envelope")
	}
	return nil
}

func jsonFieldsHaveTypes(data []byte, expected map[string]string) bool {
	if len(expected) == 0 {
		return true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for key, kind := range expected {
		raw, exists := fields[key]
		if !exists {
			return false
		}
		trimmed := bytes.TrimSpace(raw)
		switch kind {
		case "bool":
			if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
				return false
			}
		case "array":
			if len(trimmed) == 0 || trimmed[0] != '[' {
				return false
			}
		case "string":
			if len(trimmed) == 0 || trimmed[0] != '"' {
				return false
			}
		case "number":
			if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
				return false
			}
		case "integer":
			if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) || bytes.ContainsAny(trimmed, ".eE") {
				return false
			}
		}
	}
	return true
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func uniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := uniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := uniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected delimiter")
	}
}

func ParseWireActivity(data []byte) (WireActivity, error) {
	var value WireActivity
	if err := decodeExact(data, &value, "v", "type", "connection_nonce", "boot_nonce", "seq", "active", "local_stop", "session_id", "member_id", "challenge"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "seq": "integer", "active": "bool", "local_stop": "bool", "type": "string", "boot_nonce": "string", "challenge": "string"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	activityJSON, err := json.Marshal(value.Activity)
	if err != nil {
		return WireActivity{}, fmt.Errorf("invalid ride-group activity")
	}
	if _, err := ParseActivity(activityJSON); err != nil || !validUUID(value.ConnectionNonce) {
		return WireActivity{}, fmt.Errorf("invalid ride-group activity")
	}
	return value, nil
}

func ParsePosition(data []byte) (Position, error) {
	var value Position
	if err := decodeExact(data, &value, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "seq", "captured_at_ms", "timestamp_source", "lat", "lon", "accuracy_m", "heading_deg", "speed_mps", "motion_source"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "revision": "integer", "transport_epoch": "integer", "seq": "integer", "captured_at_ms": "integer", "type": "string", "session_id": "string", "stream_id": "string", "timestamp_source": "string", "lat": "number", "lon": "number", "motion_source": "string"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	if value.Version != 1 || value.Type != "ride_group_position" || !validUUID(value.SessionID) || !validUUID(value.StreamID) || !safePositive(value.Revision) || !safePositive(value.TransportEpoch) || !safePositive(value.Sequence) || !safePositive(value.CapturedAtMS) || value.TimestampSource != "producer_fix" || !validCoordinates(value.Latitude, value.Longitude) || !validAccuracy(value.AccuracyM) || !validMotion(value.HeadingDeg, value.SpeedMPS, value.MotionSource) {
		return Position{}, fmt.Errorf("invalid ride-group position")
	}
	return value, nil
}

func ParseActivity(data []byte) (Activity, error) {
	var value Activity
	if err := decodeExact(data, &value, "v", "type", "boot_nonce", "seq", "active", "local_stop", "session_id", "member_id", "challenge"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "seq": "integer", "active": "bool", "local_stop": "bool", "type": "string", "boot_nonce": "string", "challenge": "string"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	if value.Version != 1 || value.Type != "ride_group_activity" || !validUUID(value.BootNonce) || !validUUID(value.Challenge) || value.Sequence <= 0 || value.Sequence > maxSafeInteger || (value.SessionID == nil) != (value.MemberID == nil) || (value.SessionID != nil && (!validUUID(*value.SessionID) || !validUUID(*value.MemberID))) || (value.Active && value.LocalStop) {
		return Activity{}, fmt.Errorf("invalid ride-group activity")
	}
	return value, nil
}

func ParseControl(data []byte) (Control, error) {
	var value Control
	if err := decodeExact(data, &value, "v", "type", "connection_nonce", "action", "session_id", "member_id", "revision", "transport_epoch", "stream_id", "challenge", "server_time_ms", "lease_ttl_ms", "cadence_ms", "peers"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "revision": "integer", "transport_epoch": "integer", "server_time_ms": "integer", "lease_ttl_ms": "integer", "type": "string", "connection_nonce": "string", "action": "string", "challenge": "string", "peers": "array"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	if err := validateControlPeers(data, value.Peers); err != nil {
		return value, err
	}
	if err := value.Validate(); err != nil {
		return Control{}, err
	}
	return value, nil
}

func validateControlPeers(data []byte, peers []PeerDescriptor) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("invalid ride-group control")
	}
	var encodedPeers []json.RawMessage
	if err := json.Unmarshal(envelope["peers"], &encodedPeers); err != nil || len(encodedPeers) != len(peers) {
		return fmt.Errorf("invalid ride-group control")
	}
	for _, raw := range encodedPeers {
		var peer PeerDescriptor
		if err := decodeExact(raw, &peer, "peer_id", "label", "palette_index"); err != nil || !jsonFieldsHaveTypes(raw, map[string]string{"peer_id": "string", "label": "string", "palette_index": "integer"}) {
			return fmt.Errorf("invalid ride-group control")
		}
	}
	return nil
}

func (c Control) Validate() error {
	if c.Version != 1 || c.Type != "ride_group_control" || !validUUID(c.ConnectionNonce) || !validUUID(c.Challenge) || !safePositive(c.ServerTimeMS) || len(c.Peers) > 5 {
		return fmt.Errorf("invalid ride-group control")
	}
	for _, peer := range c.Peers {
		if !validUUID(peer.PeerID) || peer.PaletteIndex < 0 || peer.PaletteIndex > 5 || utf8.RuneCountInString(peer.Label) > 32 || jsonStringContentBytes(peer.Label) > 32 {
			return fmt.Errorf("invalid ride-group control")
		}
	}
	switch c.Action {
	case "grant", "renew":
		if !validUUIDPtr(c.SessionID) || !validUUIDPtr(c.MemberID) || !validUUIDPtr(c.StreamID) || !safePositive(c.Revision) || !safePositive(c.TransportEpoch) || !safePositive(c.ServerTimeMS) || c.LeaseTTLMS < 1 || c.LeaseTTLMS > 10000 || c.CadenceMS == nil || *c.CadenceMS < 1000 || *c.CadenceMS > 5000 {
			return fmt.Errorf("invalid ride-group control")
		}
	case "clear":
		if !validUUIDPtr(c.SessionID) || !validUUIDPtr(c.MemberID) || !validUUIDPtr(c.StreamID) || !safePositive(c.Revision) || !safePositive(c.TransportEpoch) || c.LeaseTTLMS != 0 || c.CadenceMS != nil || len(c.Peers) != 0 {
			return fmt.Errorf("invalid ride-group control")
		}
	case "status":
		if c.SessionID != nil || c.MemberID != nil || c.StreamID != nil || c.Revision != 0 || c.TransportEpoch != 0 || c.LeaseTTLMS != 0 || c.CadenceMS != nil || len(c.Peers) != 0 {
			return fmt.Errorf("invalid ride-group control")
		}
	default:
		return fmt.Errorf("invalid ride-group control")
	}
	return nil
}

func ParsePeerPosition(data []byte) (PeerPosition, error) {
	var value PeerPosition
	if err := decodeExact(data, &value, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "peer_stream_id", "peer_transport_epoch", "seq", "peer_id", "label", "palette_index", "captured_at_ms", "timestamp_source", "lat", "lon", "accuracy_m", "heading_deg", "speed_mps", "motion_source", "received_at_ms", "server_time_ms", "valid_for_ms"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "revision": "integer", "transport_epoch": "integer", "peer_transport_epoch": "integer", "seq": "integer", "palette_index": "integer", "captured_at_ms": "integer", "received_at_ms": "integer", "server_time_ms": "integer", "valid_for_ms": "integer", "type": "string", "session_id": "string", "stream_id": "string", "peer_stream_id": "string", "peer_id": "string", "label": "string", "timestamp_source": "string", "lat": "number", "lon": "number", "motion_source": "string"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	if value.Version != 1 || value.Type != "ride_group_peer" || !validUUID(value.SessionID) || !validUUID(value.StreamID) || !validUUID(value.PeerStreamID) || !validUUID(value.PeerID) || !safePositive(value.Revision) || !safePositive(value.TransportEpoch) || !safePositive(value.PeerTransportEpoch) || !safePositive(value.Sequence) || !safePositive(value.CapturedAtMS) || !safePositive(value.ReceivedAtMS) || !safePositive(value.ServerTimeMS) || value.ValidForMS < 0 || value.ValidForMS > 10000 || value.TimestampSource != "producer_fix" || value.PaletteIndex < 0 || value.PaletteIndex > 5 || utf8.RuneCountInString(value.Label) > 32 || jsonStringContentBytes(value.Label) > 32 || !validCoordinates(value.Latitude, value.Longitude) || !validMotion(value.HeadingDeg, value.SpeedMPS, value.MotionSource) || !validAccuracy(value.AccuracyM) {
		return PeerPosition{}, fmt.Errorf("invalid ride-group peer")
	}
	return value, nil
}

func ParsePeerStatus(data []byte) (PeerStatus, error) {
	var value PeerStatus
	if err := decodeExact(data, &value, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "peer_id", "peer_stream_id", "peer_transport_epoch", "status"); err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "revision": "integer", "transport_epoch": "integer", "peer_transport_epoch": "integer", "type": "string", "session_id": "string", "stream_id": "string", "peer_id": "string", "peer_stream_id": "string", "status": "string"}) {
		return value, fmt.Errorf("invalid ride-group envelope")
	}
	if value.Version != 1 || value.Type != "ride_group_peer_status" || !validUUID(value.SessionID) || !validUUID(value.StreamID) || !validUUID(value.PeerID) || !validUUID(value.PeerStreamID) || !safePositive(value.Revision) || !safePositive(value.TransportEpoch) || !safePositive(value.PeerTransportEpoch) || (value.Status != "no_fix" && value.Status != "inactive" && value.Status != "cleared") {
		return PeerStatus{}, fmt.Errorf("invalid ride-group peer status")
	}
	return value, nil
}

const maxSafeInteger int64 = 1<<53 - 1

func jsonStringContentBytes(value string) int {
	encodedBytes := 0
	for _, character := range value {
		switch character {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			encodedBytes += 2
		default:
			if character < 0x20 {
				encodedBytes += 6
			} else {
				encodedBytes += utf8.RuneLen(character)
			}
		}
	}
	return encodedBytes
}

func validUUID(value string) bool     { return uuidPattern.MatchString(value) }
func validUUIDPtr(value *string) bool { return value != nil && validUUID(*value) }
func safePositive(value int64) bool   { return value > 0 && value <= maxSafeInteger }
