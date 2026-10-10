package ridegroup

import "time"

// Redis delivery has its own scheduling boundary, independent of MQTT admission.
func (a *Adapter) finalRedisPayloadV2Locked(p redisPublication) ([]byte, bool) {
	if a.protocolVersion != 2 {
		return p.payload, true
	}
	now := a.currentTime()
	if !a.settingsReady || a.mqtt == nil || !a.mqtt.IsConnected() {
		return nil, false
	}
	value, err := ParseV2Message(p.payload)
	if err != nil {
		return nil, false
	}
	switch v := value.(type) {
	case LocalStopProbeV2:
		probe := a.journalProbesV2[v.Challenge]
		return p.payload, probe != nil && !probe.consumed && probe.connection == a.connection && probe.ipcEpoch == a.journalIPCEpochV2 && !now.Before(probe.issuedAt) && now.Before(probe.deadline)
	case LocalStopResultV2:
		if v.ConnectionNonce != a.connection || a.journalStateV2 == nil || a.journalStateV2.OperationID == nil || *a.journalStateV2.OperationID != v.OperationID || a.journalStateV2.JournalRevision != v.JournalRevision {
			return nil, false
		}
		if v.Result == "offer" {
			return p.payload, a.journalStateV2.Phase == "preparing"
		}
		return p.payload, a.journalStateV2.Phase == "prepared" || a.journalStateV2.Phase == "uncertain"
	case ControlV2:
		if v.ConnectionNonce != a.connection {
			return nil, false
		}
		if v.Action == "grant" || v.Action == "renew" {
			if !a.grantableV2Locked() || a.grantAt.IsZero() || now.Before(a.grantAt) || now.Sub(a.grantAt) >= a.grantTTL || !sameControlIdentityV2(a.grantV2, &v) || v.TransportEpoch != a.grantV2.TransportEpoch || v.ConsentGeneration != a.grantV2.ConsentGeneration {
				return nil, false
			}
			ttl := a.grantTTL - now.Sub(a.grantAt)
			queuedTTL := time.Duration(v.LeaseTTLMS)*time.Millisecond - now.Sub(p.queuedAt)
			if queuedTTL < ttl {
				ttl = queuedTTL
			}
			if ttl <= 0 {
				return nil, false
			}
			v.LeaseTTLMS = ttl.Milliseconds()
			data, err := EncodeControlV2(v)
			return data, err == nil
		}
		if v.Action == "clear" && a.grantV2 != nil {
			return nil, false
		}
		return p.payload, true
	case PeerPositionV2:
		if !a.receiverTupleCurrentV2Locked(v.SessionID, v.Revision, v.TransportEpoch, v.StreamID, now) {
			return nil, false
		}
		peer := a.peerStreams[v.PeerID]
		if peer.closed || peer.epoch != v.PeerTransportEpoch || peer.streamID != v.PeerStreamID || peer.sequence != v.Sequence || peer.captured != v.CapturedAtMS {
			return nil, false
		}
		age := now.Sub(p.queuedAt).Milliseconds()
		if age < 0 {
			return nil, false
		}
		v.ValidForMS -= age
		if v.ValidForMS <= 0 {
			return nil, false
		}
		data, err := MarshalCanonicalV2(v)
		return data, err == nil
	case PeerStatusV2:
		peer := a.peerStreams[v.PeerID]
		return p.payload, a.receiverTupleCurrentV2Locked(v.SessionID, v.Revision, v.TransportEpoch, v.StreamID, now) && peer.closed && peer.epoch == v.PeerTransportEpoch && peer.streamID == v.PeerStreamID
	default:
		return nil, false
	}
}
func (a *Adapter) receiverTupleCurrentV2Locked(session string, revision, epoch int64, stream string, now time.Time) bool {
	return a.grantCurrentV2Locked(now) && a.grantV2.SessionID != nil && a.grantV2.StreamID != nil && *a.grantV2.SessionID == session && a.grantV2.Revision == revision && a.grantV2.TransportEpoch == epoch && *a.grantV2.StreamID == stream
}
