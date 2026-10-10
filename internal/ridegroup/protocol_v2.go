package ridegroup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	v2ProtocolVersion     = 2
	v2HelloType           = "ride_group_hello"
	v2ActivityType        = "ride_group_activity"
	v2ControlType         = "ride_group_control"
	v2PositionType        = "ride_group_position"
	v2PeerType            = "ride_group_peer"
	v2PeerStatusType      = "ride_group_peer_status"
	v2SourceStatusType    = "ride_group_source_status"
	v2StopType            = "ride_group_stop"
	v2LocalStopType       = "ride_group_local_stop"
	v2LocalStopProbeType  = "ride_group_local_stop_probe"
	v2LocalStopResultType = "ride_group_local_stop_result"
)

type HelloV2 struct {
	Version         int      `json:"v"`
	Type            string   `json:"type"`
	ConnectionNonce string   `json:"connection_nonce"`
	Capabilities    []string `json:"capabilities"`
	Challenge       string   `json:"challenge"`
}

type ActivityV2 struct {
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

type WireActivityV2 struct {
	Version         int     `json:"v"`
	Type            string  `json:"type"`
	ConnectionNonce string  `json:"connection_nonce"`
	BootNonce       string  `json:"boot_nonce"`
	Sequence        int64   `json:"seq"`
	Active          bool    `json:"active"`
	LocalStop       bool    `json:"local_stop"`
	SessionID       *string `json:"session_id"`
	MemberID        *string `json:"member_id"`
	Challenge       string  `json:"challenge"`
}

type ControlV2 struct {
	Version                int              `json:"v"`
	Type                   string           `json:"type"`
	ConnectionNonce        string           `json:"connection_nonce"`
	Action                 string           `json:"action"`
	SessionID              *string          `json:"session_id"`
	MemberID               *string          `json:"member_id"`
	Revision               int64            `json:"revision"`
	TransportEpoch         int64            `json:"transport_epoch"`
	StreamID               *string          `json:"stream_id"`
	Challenge              string           `json:"challenge"`
	ServerTimeMS           int64            `json:"server_time_ms"`
	LeaseTTLMS             int64            `json:"lease_ttl_ms"`
	CadenceMS              *int64           `json:"cadence_ms"`
	Peers                  []PeerDescriptor `json:"peers"`
	ConsentGeneration      int64            `json:"consent_generation"`
	StopOperationID        *string          `json:"stop_operation_id"`
	StopResult             *string          `json:"stop_result"`
	StopExpectedGeneration *int64           `json:"stop_expected_generation"`
}

type PositionV2 struct {
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

type PeerPositionV2 struct {
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

type PeerStatusV2 struct {
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

type SourceStatusV2 struct {
	Version         int    `json:"v"`
	Type            string `json:"type"`
	ConnectionNonce string `json:"connection_nonce"`
	SessionID       string `json:"session_id"`
	MemberID        string `json:"member_id"`
	Revision        int64  `json:"revision"`
	TransportEpoch  int64  `json:"transport_epoch"`
	StreamID        string `json:"stream_id"`
	Sequence        int64  `json:"seq"`
	State           string `json:"state"`
	CapturedAtMS    *int64 `json:"captured_at_ms"`
}

type StopV2 struct {
	Version                   int    `json:"v"`
	Type                      string `json:"type"`
	ConnectionNonce           string `json:"connection_nonce"`
	OperationID               string `json:"operation_id"`
	ExpectedConsentGeneration *int64 `json:"expected_consent_generation"`
	Challenge                 string `json:"challenge"`
	Mode                      string `json:"mode"`
}

type LocalStopV2 struct {
	Version             int     `json:"v"`
	Type                string  `json:"type"`
	OperationID         *string `json:"operation_id"`
	JournalRevision     int64   `json:"journal_revision"`
	Phase               string  `json:"phase"`
	Blocked             bool    `json:"blocked"`
	ExpectedGeneration  *int64  `json:"expected_generation"`
	CommittedGeneration *int64  `json:"committed_generation"`
	Challenge           string  `json:"challenge"`
}

type LocalStopProbeV2 struct {
	Version   int    `json:"v"`
	Type      string `json:"type"`
	Adapter   string `json:"adapter"`
	Challenge string `json:"challenge"`
}

type LocalStopResultV2 struct {
	Version             int    `json:"v"`
	Type                string `json:"type"`
	Adapter             string `json:"adapter"`
	OperationID         string `json:"operation_id"`
	JournalRevision     int64  `json:"journal_revision"`
	Challenge           string `json:"challenge"`
	ConnectionNonce     string `json:"connection_nonce"`
	Result              string `json:"result"`
	ExpectedGeneration  *int64 `json:"expected_generation"`
	CommittedGeneration *int64 `json:"committed_generation"`
}

type LocalStopAttemptV2 struct {
	Adapter         string
	OperationID     string
	JournalRevision int64
	Challenge       string
	ConnectionNonce string
	StartedAt       time.Time
}

func decodeExactV2(data []byte, target any, keys ...string) error {
	if !validJSONScalars(data) {
		return fmt.Errorf("invalid ride-group v2 Unicode scalar encoding")
	}
	return decodeExact(data, target, keys...)
}

func ParseHelloV2(data []byte) (HelloV2, error) {
	var value HelloV2
	err := decodeExactV2(data, &value, "v", "type", "connection_nonce", "capabilities", "challenge")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "connection_nonce": "string", "capabilities": "array", "challenge": "string"}) {
		return HelloV2{}, invalidV2("hello")
	}
	if value.Version != v2ProtocolVersion || value.Type != v2HelloType || !validUUID(value.ConnectionNonce) || !validUUID(value.Challenge) || len(value.Capabilities) != 1 || value.Capabilities[0] != "ride_group_v2" {
		return HelloV2{}, invalidV2("hello")
	}
	return value, nil
}

func ParseActivityV2(data []byte) (ActivityV2, error) {
	var value ActivityV2
	err := decodeExactV2(data, &value, "v", "type", "boot_nonce", "seq", "active", "local_stop", "session_id", "member_id", "challenge")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "boot_nonce": "string", "seq": "integer", "active": "bool", "local_stop": "bool", "challenge": "string"}) {
		return ActivityV2{}, invalidV2("activity")
	}
	if err := validateActivityV2(value); err != nil {
		return ActivityV2{}, err
	}
	return value, nil
}

func ParseWireActivityV2(data []byte) (WireActivityV2, error) {
	var value WireActivityV2
	err := decodeExactV2(data, &value, "v", "type", "connection_nonce", "boot_nonce", "seq", "active", "local_stop", "session_id", "member_id", "challenge")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "connection_nonce": "string", "boot_nonce": "string", "seq": "integer", "active": "bool", "local_stop": "bool", "challenge": "string"}) {
		return WireActivityV2{}, invalidV2("activity")
	}
	if value.Version != v2ProtocolVersion || value.Type != v2ActivityType || !validUUID(value.ConnectionNonce) {
		return WireActivityV2{}, invalidV2("activity")
	}
	if err := validateActivityFieldsV2(value.BootNonce, value.Sequence, value.LocalStop, value.SessionID, value.MemberID, value.Challenge); err != nil {
		return WireActivityV2{}, err
	}
	return value, nil
}

func validateActivityV2(value ActivityV2) error {
	if value.Version != v2ProtocolVersion || value.Type != v2ActivityType {
		return invalidV2("activity")
	}
	return validateActivityFieldsV2(value.BootNonce, value.Sequence, value.LocalStop, value.SessionID, value.MemberID, value.Challenge)
}
func validateActivityFieldsV2(boot string, seq int64, localStop bool, session, member *string, challenge string) error {
	if !validUUID(boot) || !safePositive(seq) || localStop || !validUUID(challenge) {
		return invalidV2("activity")
	}
	if (session == nil) != (member == nil) || (session != nil && (!validUUID(*session) || !validUUID(*member))) {
		return invalidV2("activity")
	}
	return nil
}

func ParseControlV2(data []byte) (ControlV2, error) {
	var value ControlV2
	err := decodeExactV2(data, &value, "v", "type", "connection_nonce", "action", "session_id", "member_id", "revision", "transport_epoch", "stream_id", "challenge", "server_time_ms", "lease_ttl_ms", "cadence_ms", "peers", "consent_generation", "stop_operation_id", "stop_result", "stop_expected_generation")
	if err != nil {
		return ControlV2{}, invalidV2("control")
	}
	types := map[string]string{"v": "integer", "type": "string", "connection_nonce": "string", "action": "string", "revision": "integer", "transport_epoch": "integer", "challenge": "string", "server_time_ms": "integer", "lease_ttl_ms": "integer", "peers": "array", "consent_generation": "integer"}
	if !jsonFieldsHaveTypes(data, types) || !nullableJSONFieldType(data, "cadence_ms", "integer") || !nullableJSONFieldType(data, "stop_expected_generation", "integer") {
		return ControlV2{}, invalidV2("control")
	}
	for _, peer := range value.Peers {
		if !validPeerDescriptorV2(peer) {
			return ControlV2{}, invalidV2("control peer")
		}
	}
	if err := validateControlV2(value, true); err != nil {
		return ControlV2{}, err
	}
	return value, nil
}

func validateControlV2(c ControlV2, checkLabels bool) error {
	if c.Version != v2ProtocolVersion || c.Type != v2ControlType || !validUUID(c.ConnectionNonce) || !validUUID(c.Challenge) || c.ConsentGeneration < 0 || c.ConsentGeneration > maxSafeInteger || !safeNonnegative(c.Revision) || !safeNonnegative(c.TransportEpoch) || !safeNonnegative(c.ServerTimeMS) || !safeNonnegative(c.LeaseTTLMS) || c.Peers == nil || len(c.Peers) > 5 {
		return invalidV2("control")
	}
	if c.StopOperationID != nil && !validUUID(*c.StopOperationID) {
		return invalidV2("control")
	}
	if c.StopResult != nil && !oneOf(*c.StopResult, "committed", "not_found", "superseded", "expired", "unconfirmed", "error") {
		return invalidV2("control")
	}
	if c.StopExpectedGeneration != nil && !safeNonnegative(*c.StopExpectedGeneration) {
		return invalidV2("control")
	}
	for _, p := range c.Peers {
		if !validUUID(p.PeerID) || p.PaletteIndex < 0 || p.PaletteIndex > 5 || !utf8.ValidString(p.Label) || (checkLabels && !validPeerLabel(p.Label)) {
			return invalidV2("control peer")
		}
	}
	noStop := c.StopOperationID == nil && c.StopResult == nil && c.StopExpectedGeneration == nil
	switch c.Action {
	case "grant", "renew":
		if !noStop || !validUUIDPtr(c.SessionID) || !validUUIDPtr(c.MemberID) || !validUUIDPtr(c.StreamID) || !safePositive(c.Revision) || !safePositive(c.TransportEpoch) || !safePositive(c.ServerTimeMS) || c.LeaseTTLMS < 1 || c.LeaseTTLMS > 10000 || c.CadenceMS == nil || *c.CadenceMS < 1 || *c.CadenceMS > 5000 {
			return invalidV2("control")
		}
		if checkLabels {
			for _, p := range c.Peers {
				if utf8.RuneCountInString(p.Label) > 32 || jsonStringContentBytes(p.Label) > 32 {
					return invalidV2("control label")
				}
			}
		}
	case "clear":
		if !noStop || !validUUIDPtr(c.SessionID) || !validUUIDPtr(c.MemberID) || !validUUIDPtr(c.StreamID) || !safePositive(c.Revision) || !safePositive(c.TransportEpoch) || !safePositive(c.ServerTimeMS) || c.LeaseTTLMS != 0 || c.CadenceMS != nil || len(c.Peers) != 0 {
			return invalidV2("control")
		}
	case "status":
		if c.SessionID != nil || c.MemberID != nil || c.StreamID != nil || c.Revision != 0 || c.TransportEpoch != 0 || !safePositive(c.ServerTimeMS) || c.LeaseTTLMS != 0 || c.CadenceMS != nil || len(c.Peers) != 0 {
			return invalidV2("control")
		}
		if c.StopResult == nil {
			if !noStop {
				return invalidV2("control")
			}
			return nil
		}
		if c.StopOperationID == nil {
			return invalidV2("control")
		}
		switch *c.StopResult {
		case "committed", "superseded", "unconfirmed":
			if c.StopExpectedGeneration == nil || c.ConsentGeneration <= *c.StopExpectedGeneration {
				return invalidV2("control stop result")
			}
		case "not_found", "expired":
			if c.StopExpectedGeneration != nil {
				return invalidV2("control stop result")
			}
		case "error":
		}
	default:
		return invalidV2("control action")
	}
	return nil
}

func validPeerDescriptorV2(p PeerDescriptor) bool {
	return validUUID(p.PeerID) && validPeerLabel(p.Label) && p.PaletteIndex >= 0 && p.PaletteIndex <= 5
}

func validPeerLabel(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= 32 && jsonStringContentBytes(value) <= 32
}

func ParsePositionV2(data []byte) (PositionV2, error) {
	var v PositionV2
	err := decodeExactV2(data, &v, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "seq", "captured_at_ms", "timestamp_source", "lat", "lon", "accuracy_m", "heading_deg", "speed_mps", "motion_source")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "session_id": "string", "revision": "integer", "transport_epoch": "integer", "stream_id": "string", "seq": "integer", "captured_at_ms": "integer", "timestamp_source": "string", "lat": "number", "lon": "number", "motion_source": "string"}) || !nullableJSONFieldType(data, "accuracy_m", "number") || !nullableJSONFieldType(data, "heading_deg", "number") || !nullableJSONFieldType(data, "speed_mps", "number") {
		return PositionV2{}, invalidV2("position")
	}
	if v.Version != v2ProtocolVersion || v.Type != v2PositionType || !validUUID(v.SessionID) || !validUUID(v.StreamID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.Sequence) || !safePositive(v.CapturedAtMS) || v.TimestampSource != "producer_fix" || !finite(v.Latitude) || v.Latitude < -90 || v.Latitude > 90 || !finite(v.Longitude) || v.Longitude < -180 || v.Longitude > 180 {
		return PositionV2{}, invalidV2("position")
	}
	if !validAccuracy(v.AccuracyM) || !validMotion(v.HeadingDeg, v.SpeedMPS, v.MotionSource) {
		return PositionV2{}, invalidV2("position")
	}
	return v, nil
}

func ParsePeerPositionV2(data []byte) (PeerPositionV2, error) {
	var v PeerPositionV2
	err := decodeExactV2(data, &v, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "peer_stream_id", "peer_transport_epoch", "seq", "peer_id", "label", "palette_index", "captured_at_ms", "timestamp_source", "lat", "lon", "accuracy_m", "heading_deg", "speed_mps", "motion_source", "received_at_ms", "server_time_ms", "valid_for_ms")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "session_id": "string", "revision": "integer", "transport_epoch": "integer", "stream_id": "string", "peer_stream_id": "string", "peer_transport_epoch": "integer", "seq": "integer", "peer_id": "string", "label": "string", "palette_index": "integer", "captured_at_ms": "integer", "timestamp_source": "string", "lat": "number", "lon": "number", "motion_source": "string", "received_at_ms": "integer", "server_time_ms": "integer", "valid_for_ms": "integer"}) || !nullableJSONFieldType(data, "accuracy_m", "number") || !nullableJSONFieldType(data, "heading_deg", "number") || !nullableJSONFieldType(data, "speed_mps", "number") {
		return PeerPositionV2{}, invalidV2("peer")
	}
	if v.Version != 2 || v.Type != v2PeerType || !validUUID(v.SessionID) || !validUUID(v.StreamID) || !validUUID(v.PeerStreamID) || !validUUID(v.PeerID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.PeerTransportEpoch) || !safePositive(v.Sequence) || !safePositive(v.CapturedAtMS) || v.TimestampSource != "producer_fix" || !safePositive(v.ReceivedAtMS) || !safePositive(v.ServerTimeMS) || v.ValidForMS < 0 || v.ValidForMS > 10000 || v.PaletteIndex < 0 || v.PaletteIndex > 5 || !validPeerLabel(v.Label) || !validCoordinates(v.Latitude, v.Longitude) || !validMotion(v.HeadingDeg, v.SpeedMPS, v.MotionSource) || !validAccuracy(v.AccuracyM) {
		return PeerPositionV2{}, invalidV2("peer")
	}
	return v, nil
}

func ParsePeerStatusV2(data []byte) (PeerStatusV2, error) {
	var v PeerStatusV2
	err := decodeExactV2(data, &v, "v", "type", "session_id", "revision", "transport_epoch", "stream_id", "peer_id", "peer_stream_id", "peer_transport_epoch", "status")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "session_id": "string", "revision": "integer", "transport_epoch": "integer", "stream_id": "string", "peer_id": "string", "peer_stream_id": "string", "peer_transport_epoch": "integer", "status": "string"}) {
		return PeerStatusV2{}, invalidV2("peer status")
	}
	if v.Version != 2 || v.Type != v2PeerStatusType || !validUUID(v.SessionID) || !validUUID(v.StreamID) || !validUUID(v.PeerID) || !validUUID(v.PeerStreamID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.PeerTransportEpoch) || !oneOf(v.Status, "no_fix", "inactive", "cleared") {
		return PeerStatusV2{}, invalidV2("peer status")
	}
	return v, nil
}

func ParseSourceStatusV2(data []byte) (SourceStatusV2, error) {
	var v SourceStatusV2
	err := decodeExactV2(data, &v, "v", "type", "connection_nonce", "session_id", "member_id", "revision", "transport_epoch", "stream_id", "seq", "state", "captured_at_ms")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "connection_nonce": "string", "session_id": "string", "member_id": "string", "revision": "integer", "transport_epoch": "integer", "stream_id": "string", "seq": "integer", "state": "string"}) || !nullableJSONFieldType(data, "captured_at_ms", "integer") {
		return SourceStatusV2{}, invalidV2("source status")
	}
	if v.Version != 2 || v.Type != v2SourceStatusType || !validUUID(v.ConnectionNonce) || !validUUID(v.SessionID) || !validUUID(v.MemberID) || !validUUID(v.StreamID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.Sequence) {
		return SourceStatusV2{}, invalidV2("source status")
	}
	switch v.State {
	case "no_fix":
		if v.CapturedAtMS != nil {
			return SourceStatusV2{}, invalidV2("source status")
		}
	case "fix_available":
		if v.CapturedAtMS == nil || !safePositive(*v.CapturedAtMS) {
			return SourceStatusV2{}, invalidV2("source status")
		}
	default:
		return SourceStatusV2{}, invalidV2("source status")
	}
	return v, nil
}

func ParseStopV2(data []byte) (StopV2, error) {
	var v StopV2
	err := decodeExactV2(data, &v, "v", "type", "connection_nonce", "operation_id", "expected_consent_generation", "challenge", "mode")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "connection_nonce": "string", "operation_id": "string", "challenge": "string", "mode": "string"}) || !nullableJSONFieldType(data, "expected_consent_generation", "integer") {
		return StopV2{}, invalidV2("stop")
	}
	if v.Version != 2 || v.Type != v2StopType || !validUUID(v.ConnectionNonce) || !validUUID(v.OperationID) || !validUUID(v.Challenge) || !oneOf(v.Mode, "apply", "query") || (v.Mode == "apply" && v.ExpectedConsentGeneration == nil) || (v.ExpectedConsentGeneration != nil && !safeNonnegative(*v.ExpectedConsentGeneration)) {
		return StopV2{}, invalidV2("stop")
	}
	return v, nil
}

func ParseLocalStopV2(data []byte) (LocalStopV2, error) {
	var v LocalStopV2
	err := decodeExactV2(data, &v, "v", "type", "operation_id", "journal_revision", "phase", "blocked", "expected_generation", "committed_generation", "challenge")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "journal_revision": "integer", "phase": "string", "blocked": "boolean", "challenge": "string"}) || !nullableJSONFieldType(data, "expected_generation", "integer") || !nullableJSONFieldType(data, "committed_generation", "integer") {
		return LocalStopV2{}, invalidV2("local stop")
	}
	if v.Version != 2 || v.Type != v2LocalStopType || !validUUID(v.Challenge) || !safeNonnegative(v.JournalRevision) || (v.OperationID != nil && !validUUID(*v.OperationID)) || !validOptionalGeneration(v.ExpectedGeneration) || !validOptionalGeneration(v.CommittedGeneration) {
		return LocalStopV2{}, invalidV2("local stop")
	}
	switch v.Phase {
	case "idle":
		if v.Blocked || v.OperationID != nil || v.ExpectedGeneration != nil || v.CommittedGeneration != nil || v.JournalRevision == 0 {
			return LocalStopV2{}, invalidV2("local stop")
		}
	case "preparing":
		if !v.Blocked || v.OperationID == nil || v.ExpectedGeneration != nil || v.CommittedGeneration != nil || v.JournalRevision == 0 {
			return LocalStopV2{}, invalidV2("local stop")
		}
	case "prepared":
		if !v.Blocked || v.OperationID == nil || v.ExpectedGeneration == nil || v.CommittedGeneration != nil || v.JournalRevision == 0 {
			return LocalStopV2{}, invalidV2("local stop")
		}
	case "confirmed":
		if !v.Blocked || v.OperationID == nil || !advancingGenerations(v.ExpectedGeneration, v.CommittedGeneration) || v.JournalRevision == 0 {
			return LocalStopV2{}, invalidV2("local stop")
		}
	case "uncertain":
		if !v.Blocked || (v.JournalRevision == 0 && (v.OperationID != nil || v.ExpectedGeneration != nil || v.CommittedGeneration != nil)) || (v.JournalRevision > 0 && v.CommittedGeneration != nil && !advancingGenerations(v.ExpectedGeneration, v.CommittedGeneration)) {
			return LocalStopV2{}, invalidV2("local stop")
		}
	default:
		return LocalStopV2{}, invalidV2("local stop")
	}
	return v, nil
}

func ParseLocalStopProbeV2(data []byte) (LocalStopProbeV2, error) {
	var v LocalStopProbeV2
	err := decodeExactV2(data, &v, "v", "type", "adapter", "challenge")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "adapter": "string", "challenge": "string"}) {
		return LocalStopProbeV2{}, invalidV2("local stop probe")
	}
	if v.Version != 2 || v.Type != v2LocalStopProbeType || !oneOf(v.Adapter, "mqtt", "uplink") || !validUUID(v.Challenge) {
		return LocalStopProbeV2{}, invalidV2("local stop probe")
	}
	return v, nil
}

func ParseLocalStopResultV2(data []byte) (LocalStopResultV2, error) {
	var v LocalStopResultV2
	err := decodeExactV2(data, &v, "v", "type", "adapter", "operation_id", "journal_revision", "challenge", "connection_nonce", "result", "expected_generation", "committed_generation")
	if err != nil || !jsonFieldsHaveTypes(data, map[string]string{"v": "integer", "type": "string", "adapter": "string", "operation_id": "string", "journal_revision": "integer", "challenge": "string", "connection_nonce": "string", "result": "string"}) || !nullableJSONFieldType(data, "expected_generation", "integer") || !nullableJSONFieldType(data, "committed_generation", "integer") {
		return LocalStopResultV2{}, invalidV2("local stop result")
	}
	if v.Version != 2 || v.Type != v2LocalStopResultType || !oneOf(v.Adapter, "mqtt", "uplink") || !validUUID(v.OperationID) || !safePositive(v.JournalRevision) || !validUUID(v.Challenge) || !validUUID(v.ConnectionNonce) || !validOptionalGeneration(v.ExpectedGeneration) || !validOptionalGeneration(v.CommittedGeneration) {
		return LocalStopResultV2{}, invalidV2("local stop result")
	}
	switch v.Result {
	case "offer":
		if v.ExpectedGeneration == nil || v.CommittedGeneration != nil {
			return LocalStopResultV2{}, invalidV2("local stop result")
		}
	case "committed", "superseded", "unconfirmed":
		if !advancingGenerations(v.ExpectedGeneration, v.CommittedGeneration) {
			return LocalStopResultV2{}, invalidV2("local stop result")
		}
	case "not_found", "expired":
		if v.CommittedGeneration != nil {
			return LocalStopResultV2{}, invalidV2("local stop result")
		}
	case "error":
		if v.CommittedGeneration != nil && !advancingGenerations(v.ExpectedGeneration, v.CommittedGeneration) {
			return LocalStopResultV2{}, invalidV2("local stop result")
		}
	default:
		return LocalStopResultV2{}, invalidV2("local stop result")
	}
	return v, nil
}

func MatchesLocalStopAttemptV2(result LocalStopResultV2, attempt LocalStopAttemptV2, currentNonce string, now time.Time) bool {
	return attempt.Adapter == result.Adapter && (attempt.Adapter == "mqtt" || attempt.Adapter == "uplink") && attempt.OperationID == result.OperationID && attempt.JournalRevision == result.JournalRevision && attempt.Challenge == result.Challenge && attempt.ConnectionNonce == result.ConnectionNonce && currentNonce == attempt.ConnectionNonce && validUUID(attempt.OperationID) && validUUID(attempt.Challenge) && validUUID(currentNonce) && safePositive(attempt.JournalRevision) && !attempt.StartedAt.IsZero() && !now.Before(attempt.StartedAt) && now.Sub(attempt.StartedAt) <= 10*time.Second
}

func ParseV2Message(data []byte) (any, error) {
	if !validJSONScalars(data) {
		return nil, invalidV2("envelope")
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, invalidV2("envelope")
	}
	switch header.Type {
	case v2HelloType:
		return ParseHelloV2(data)
	case v2ActivityType:
		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) != nil {
			return nil, invalidV2("activity")
		}
		if _, ok := raw["connection_nonce"]; ok {
			return ParseWireActivityV2(data)
		}
		return ParseActivityV2(data)
	case v2ControlType:
		return ParseControlV2(data)
	case v2PositionType:
		return ParsePositionV2(data)
	case v2PeerType:
		return ParsePeerPositionV2(data)
	case v2PeerStatusType:
		return ParsePeerStatusV2(data)
	case v2SourceStatusType:
		return ParseSourceStatusV2(data)
	case v2StopType:
		return ParseStopV2(data)
	case v2LocalStopType:
		return ParseLocalStopV2(data)
	case v2LocalStopProbeType:
		return ParseLocalStopProbeV2(data)
	case v2LocalStopResultType:
		return ParseLocalStopResultV2(data)
	default:
		return nil, invalidV2("type")
	}
}

func MarshalCanonicalV2(value any) ([]byte, error) {
	if err := validateV2Value(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if _, err := ParseV2Message(encoded); err != nil {
		return nil, err
	}
	canonical, err := canonicalizeJSONStringEscapes(encoded)
	if err != nil || len(canonical) > MaxEnvelopeBytes {
		return nil, invalidV2("encoded size")
	}
	return canonical, nil
}

func EncodeControlV2(value ControlV2) ([]byte, error)               { return MarshalCanonicalV2(value) }
func EncodeHelloV2(value HelloV2) ([]byte, error)                   { return MarshalCanonicalV2(value) }
func EncodeActivityV2(value ActivityV2) ([]byte, error)             { return MarshalCanonicalV2(value) }
func EncodeWireActivityV2(value WireActivityV2) ([]byte, error)     { return MarshalCanonicalV2(value) }
func EncodePositionV2(value PositionV2) ([]byte, error)             { return MarshalCanonicalV2(value) }
func EncodePeerPositionV2(value PeerPositionV2) ([]byte, error)     { return MarshalCanonicalV2(value) }
func EncodePeerStatusV2(value PeerStatusV2) ([]byte, error)         { return MarshalCanonicalV2(value) }
func EncodeSourceStatusV2(value SourceStatusV2) ([]byte, error)     { return MarshalCanonicalV2(value) }
func EncodeStopV2(value StopV2) ([]byte, error)                     { return MarshalCanonicalV2(value) }
func EncodeLocalStopV2(value LocalStopV2) ([]byte, error)           { return MarshalCanonicalV2(value) }
func EncodeLocalStopProbeV2(value LocalStopProbeV2) ([]byte, error) { return MarshalCanonicalV2(value) }
func EncodeLocalStopResultV2(value LocalStopResultV2) ([]byte, error) {
	return MarshalCanonicalV2(value)
}

func FitControlLabelsV2(control ControlV2) (ControlV2, error) {
	if err := validateControlV2(control, false); err != nil {
		return ControlV2{}, err
	}
	labels := make([][]rune, len(control.Peers))
	for i := range control.Peers {
		labels[i] = []rune(control.Peers[i].Label)
		control.Peers[i].Label = ""
	}
	if _, err := EncodeControlV2(control); err != nil {
		return ControlV2{}, err
	}
	positions := make([]int, len(labels))
	for {
		progress := false
		for i := range labels {
			if positions[i] >= len(labels[i]) {
				continue
			}
			progress = true
			next := string(labels[i][positions[i]])
			candidate := control.Peers[i].Label + next
			if !utf8.ValidString(candidate) || utf8.RuneCountInString(candidate) > 32 || jsonStringContentBytes(candidate) > 32 {
				positions[i] = len(labels[i])
				continue
			}
			previous := control.Peers[i].Label
			control.Peers[i].Label = candidate
			if _, err := EncodeControlV2(control); err != nil {
				control.Peers[i].Label = previous
				positions[i] = len(labels[i])
				continue
			}
			positions[i]++
		}
		if !progress {
			break
		}
	}
	return control, nil
}

func validateV2Value(value any) error {
	switch v := value.(type) {
	case HelloV2:
		_, err := ParseHelloV2(mustMarshal(v))
		return err
	case ActivityV2:
		if err := validateActivityV2(v); err != nil {
			return err
		}
	case WireActivityV2:
		if v.Version != 2 || v.Type != v2ActivityType || !validUUID(v.ConnectionNonce) {
			return invalidV2("activity")
		}
		if err := validateActivityFieldsV2(v.BootNonce, v.Sequence, v.LocalStop, v.SessionID, v.MemberID, v.Challenge); err != nil {
			return err
		}
	case ControlV2:
		return validateControlV2(v, true)
	case PositionV2:
		if !validPositionV2Value(v) {
			return invalidV2("position")
		}
	case PeerPositionV2:
		if !validPeerPositionV2Value(v) {
			return invalidV2("peer")
		}
	case PeerStatusV2:
		if !validPeerStatusV2Value(v) {
			return invalidV2("peer status")
		}
	case SourceStatusV2:
		if !validSourceStatusV2Value(v) {
			return invalidV2("source status")
		}
	case StopV2:
		if !validStopV2Value(v) {
			return invalidV2("stop")
		}
	case LocalStopV2:
		if !validLocalStopV2Value(v) {
			return invalidV2("local stop")
		}
	case LocalStopProbeV2:
		if v.Version != 2 || v.Type != v2LocalStopProbeType || !oneOf(v.Adapter, "mqtt", "uplink") || !validUUID(v.Challenge) {
			return invalidV2("local stop probe")
		}
	case LocalStopResultV2:
		if !validLocalStopResultV2Value(v) {
			return invalidV2("local stop result")
		}
	default:
		return fmt.Errorf("unsupported ride-group v2 value %T", value)
	}
	return nil
}

func mustMarshal(value any) []byte { data, _ := json.Marshal(value); return data }
func validPositionV2Value(v PositionV2) bool {
	b, err := EncodePositionShapeV2(v)
	return err == nil && b
}
func EncodePositionShapeV2(v PositionV2) (bool, error) {
	if v.Version != 2 || v.Type != v2PositionType || !validUUID(v.SessionID) || !validUUID(v.StreamID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.Sequence) || !safePositive(v.CapturedAtMS) || v.TimestampSource != "producer_fix" || !finite(v.Latitude) || v.Latitude < -90 || v.Latitude > 90 || !finite(v.Longitude) || v.Longitude < -180 || v.Longitude > 180 || !validAccuracy(v.AccuracyM) || !validMotion(v.HeadingDeg, v.SpeedMPS, v.MotionSource) {
		return false, invalidV2("position")
	}
	return true, nil
}
func validPeerPositionV2Value(v PeerPositionV2) bool {
	return v.Version == 2 && v.Type == v2PeerType && validUUID(v.SessionID) && validUUID(v.StreamID) && validUUID(v.PeerStreamID) && validUUID(v.PeerID) && safePositive(v.Revision) && safePositive(v.TransportEpoch) && safePositive(v.PeerTransportEpoch) && safePositive(v.Sequence) && safePositive(v.CapturedAtMS) && v.TimestampSource == "producer_fix" && safePositive(v.ReceivedAtMS) && safePositive(v.ServerTimeMS) && v.ValidForMS >= 0 && v.ValidForMS <= 10000 && v.PaletteIndex >= 0 && v.PaletteIndex <= 5 && validPeerLabel(v.Label) && finite(v.Latitude) && v.Latitude >= -90 && v.Latitude <= 90 && finite(v.Longitude) && v.Longitude >= -180 && v.Longitude <= 180 && validMotion(v.HeadingDeg, v.SpeedMPS, v.MotionSource) && validAccuracy(v.AccuracyM)
}
func validPeerStatusV2Value(v PeerStatusV2) bool {
	return v.Version == 2 && v.Type == v2PeerStatusType && validUUID(v.SessionID) && validUUID(v.StreamID) && validUUID(v.PeerID) && validUUID(v.PeerStreamID) && safePositive(v.Revision) && safePositive(v.TransportEpoch) && safePositive(v.PeerTransportEpoch) && oneOf(v.Status, "no_fix", "inactive", "cleared")
}
func validSourceStatusV2Value(v SourceStatusV2) bool {
	if v.Version != 2 || v.Type != v2SourceStatusType || !validUUID(v.ConnectionNonce) || !validUUID(v.SessionID) || !validUUID(v.MemberID) || !validUUID(v.StreamID) || !safePositive(v.Revision) || !safePositive(v.TransportEpoch) || !safePositive(v.Sequence) {
		return false
	}
	if v.State == "no_fix" {
		return v.CapturedAtMS == nil
	}
	return v.State == "fix_available" && v.CapturedAtMS != nil && safePositive(*v.CapturedAtMS)
}
func validStopV2Value(v StopV2) bool {
	return v.Version == 2 && v.Type == v2StopType && validUUID(v.ConnectionNonce) && validUUID(v.OperationID) && validUUID(v.Challenge) && oneOf(v.Mode, "apply", "query") && (v.Mode != "apply" || v.ExpectedConsentGeneration != nil) && (v.ExpectedConsentGeneration == nil || safeNonnegative(*v.ExpectedConsentGeneration))
}
func validLocalStopV2Value(v LocalStopV2) bool {
	return v.Version == 2 && v.Type == v2LocalStopType && validUUID(v.Challenge) && safeNonnegative(v.JournalRevision) && (v.OperationID == nil || validUUID(*v.OperationID)) && validOptionalGeneration(v.ExpectedGeneration) && validOptionalGeneration(v.CommittedGeneration)
}
func validLocalStopResultV2Value(v LocalStopResultV2) bool {
	return v.Version == 2 && v.Type == v2LocalStopResultType && oneOf(v.Adapter, "mqtt", "uplink") && validUUID(v.OperationID) && safePositive(v.JournalRevision) && validUUID(v.Challenge) && validUUID(v.ConnectionNonce) && validOptionalGeneration(v.ExpectedGeneration) && validOptionalGeneration(v.CommittedGeneration)
}

func validOptionalGeneration(v *int64) bool { return v == nil || safeNonnegative(*v) }
func advancingGenerations(expected, committed *int64) bool {
	return expected != nil && committed != nil && safeNonnegative(*expected) && safeNonnegative(*committed) && *committed > *expected
}
func nullableJSONFieldType(data []byte, name, want string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	raw, ok := object[name]
	if !ok {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	return jsonFieldsHaveTypes([]byte("{"+strconv.Quote(name)+":"+string(raw)+"}"), map[string]string{name: want})
}
func safeNonnegative(value int64) bool { return value >= 0 && value <= maxSafeInteger }
func finite(value float64) bool        { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func validOptionalRange(value *float64, min, max float64) bool {
	return value == nil || (finite(*value) && *value >= min && *value <= max)
}
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
func invalidV2(subject string) error { return fmt.Errorf("invalid ride-group v2 %s", subject) }

func validJSONScalars(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	inString := false
	for i := 0; i < len(data); {
		b := data[i]
		if !inString {
			if b == '"' {
				inString = true
			}
			i++
			continue
		}
		if b == '"' {
			inString = false
			i++
			continue
		}
		if b != '\\' {
			if b < 0x20 {
				return false
			}
			_, size := utf8.DecodeRune(data[i:])
			if size == 0 {
				return false
			}
			i += size
			continue
		}
		if i+1 >= len(data) {
			return false
		}
		if data[i+1] != 'u' {
			i += 2
			continue
		}
		first, ok := hex4(data[i+2:])
		if !ok {
			return false
		}
		i += 6
		if first >= 0xD800 && first <= 0xDBFF {
			if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
				return false
			}
			second, ok := hex4(data[i+2:])
			if !ok || second < 0xDC00 || second > 0xDFFF {
				return false
			}
			i += 6
		} else if first >= 0xDC00 && first <= 0xDFFF {
			return false
		}
	}
	return !inString
}
func hex4(data []byte) (uint16, bool) {
	if len(data) < 4 {
		return 0, false
	}
	value := uint16(0)
	for _, b := range data[:4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			value |= uint16(b - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}

func canonicalizeJSONStringEscapes(data []byte) ([]byte, error) {
	var out bytes.Buffer
	inString := false
	for i := 0; i < len(data); {
		b := data[i]
		if !inString {
			out.WriteByte(b)
			if b == '"' {
				inString = true
			}
			i++
			continue
		}
		if b == '"' {
			out.WriteByte(b)
			inString = false
			i++
			continue
		}
		if b != '\\' {
			out.WriteByte(b)
			i++
			continue
		}
		if i+1 >= len(data) {
			return nil, invalidV2("canonical string")
		}
		next := data[i+1]
		if next != 'u' {
			out.Write(data[i : i+2])
			i += 2
			continue
		}
		code, ok := hex4(data[i+2:])
		if !ok {
			return nil, invalidV2("canonical string")
		}
		switch code {
		case 0x003c, 0x003e, 0x0026, 0x2028, 0x2029:
			out.WriteRune(rune(code))
		default:
			out.Write(data[i : i+6])
		}
		i += 6
	}
	if inString {
		return nil, invalidV2("canonical string")
	}
	return out.Bytes(), nil
}

func CanonicalV2String(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, invalidV2("string encoding")
	}
	return canonicalizeJSONStringEscapes(mustMarshal(value))
}

func normalizedJSON(data []byte) bool { return strings.TrimSpace(string(data)) == string(data) }
