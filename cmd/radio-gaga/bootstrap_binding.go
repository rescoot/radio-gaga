package main

import (
	"encoding/json"
	"fmt"
)

// The installation ID survives restarts; the nonce addresses only this process.
// Both must match before a retained or repeated delivery can touch the config.
type bootstrapBinding struct {
	installationID string
	nonce          string
}

func (b bootstrapBinding) topics(username string) (announce, config, result string) {
	prefix := "claim/" + username
	announce = prefix + "/announce"
	if b.installationID != "" {
		prefix += "/installations/" + b.installationID
	}
	return announce, prefix + "/config", prefix + "/result"
}

func (b bootstrapBinding) delivery(payload []byte) (txnReplaceEnvelope, error) {
	var envelope txnReplaceEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return envelope, fmt.Errorf("invalid configuration envelope")
	}
	if envelope.Command != "txn:replace" || envelope.Params.TxnID == "" || envelope.Params.ConfigYAML == "" ||
		(envelope.Params.Kind != "" && envelope.Params.Kind != "config") {
		return envelope, fmt.Errorf("incomplete configuration transaction")
	}
	if b.installationID != "" {
		if envelope.Params.InstallationID != b.installationID || envelope.Params.Nonce != b.nonce {
			return envelope, fmt.Errorf("configuration does not address this installation and process")
		}
	} else if envelope.Params.InstallationID != "" {
		return envelope, fmt.Errorf("installation-bound configuration requires bootstrap.installation_id")
	}
	return envelope, nil
}

func (b bootstrapBinding) result(txnID, status, reason string) []byte {
	result := map[string]string{"status": status, "txn_id": txnID}
	if reason != "" {
		result["reason"] = reason
	}
	if b.installationID != "" {
		result["installation_id"] = b.installationID
		result["nonce"] = b.nonce
	}
	body, _ := json.Marshal(result)
	return body
}
