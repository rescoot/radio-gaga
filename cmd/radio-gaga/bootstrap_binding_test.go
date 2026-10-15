package main

import (
	"encoding/json"
	"testing"

	"radio-gaga/internal/models"
	"radio-gaga/internal/modeminfo"
)

const (
	testInstallationA = "797ad894-3fb7-41bc-af68-e534a92b2546"
	testInstallationB = "48c65b57-3227-4876-a5a2-3fdb5626cf29"
)

func boundDelivery(installationID, nonce, txnID, config string) []byte {
	envelope := txnReplaceEnvelope{Command: "txn:replace"}
	envelope.Params.InstallationID = installationID
	envelope.Params.Nonce = nonce
	envelope.Params.TxnID = txnID
	envelope.Params.Kind = "config"
	envelope.Params.ConfigYAML = config
	envelope.Params.Restart = true
	body, _ := json.Marshal(envelope)
	return body
}

func TestBootstrapBindingTopics(t *testing.T) {
	for _, id := range []string{"", testInstallationA, testInstallationB} {
		binding := bootstrapBinding{installationID: id, nonce: "session"}
		announce, config, result := binding.topics("186856c4dd2e60a0")
		prefix := "claim/186856c4dd2e60a0"
		if announce != prefix+"/announce" {
			t.Fatal("announcements must use the account announcement topic")
		}
		if id != "" {
			prefix += "/installations/" + id
		}
		if config != prefix+"/config" || result != prefix+"/result" {
			t.Fatalf("unexpected topics: %s %s", config, result)
		}
	}
}

func TestBootstrapBindingRejectsUnaddressedDeliveries(t *testing.T) {
	binding := bootstrapBinding{installationID: testInstallationA, nonce: "current-process"}
	for _, tc := range []struct {
		name, id, nonce, txnID, config string
		accepted                       bool
	}{
		{"matching", testInstallationA, binding.nonce, "txn-1", "config", true},
		{"different installation", testInstallationB, binding.nonce, "txn-1", "config", false},
		{"missing installation", "", binding.nonce, "txn-1", "config", false},
		{"previous process", testInstallationA, "previous-process", "txn-1", "config", false},
		{"missing nonce", testInstallationA, "", "txn-1", "config", false},
		{"unbound", "", "", "txn-1", "config", false},
		{"missing transaction", testInstallationA, binding.nonce, "", "config", false},
		{"missing config", testInstallationA, binding.nonce, "txn-1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := binding.delivery(boundDelivery(tc.id, tc.nonce, tc.txnID, tc.config))
			if (err == nil) != tc.accepted {
				t.Fatalf("delivery error = %v, want accepted=%v", err, tc.accepted)
			}
		})
	}
	for _, body := range []string{
		`not JSON`, `null`, `[]`,
		`{"command":"redis","params":{"txn_id":"t","config_yaml":"c"}}`,
		`{"command":"txn:replace","params":{"kind":"binary","txn_id":"t","config_yaml":"c"}}`,
	} {
		if _, err := binding.delivery([]byte(body)); err == nil {
			t.Fatalf("accepted malformed delivery %s", body)
		}
	}
}

func TestBootstrapUnboundCompatibility(t *testing.T) {
	binding := bootstrapBinding{nonce: "process"}
	if _, err := binding.delivery(boundDelivery("", "", "txn-1", "config")); err != nil {
		t.Fatal(err)
	}
	if _, err := binding.delivery(boundDelivery(testInstallationA, binding.nonce, "txn-1", "config")); err == nil {
		t.Fatal("unbound session accepted an installation-bound delivery")
	}
	var result map[string]string
	if err := json.Unmarshal(binding.result("txn-1", "success", ""), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result["status"] != "success" || result["txn_id"] != "txn-1" {
		t.Fatalf("unexpected unbound result: %v", result)
	}
}

func TestBootstrapBoundAnnouncementAndResult(t *testing.T) {
	binding := bootstrapBinding{installationID: testInstallationA, nonce: "process"}
	identity := modeminfo.Identity{IMEI: "123456789012345", ICCID: "8949000000000000001", IMSI: "901280000000001"}
	var announcement map[string]string
	body := announcePayload(identity.IMEI, "mdb", "dbc", "test", "librescoot", "My scooter", binding.nonce, binding.installationID, identity)
	if err := json.Unmarshal(body, &announcement); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"installation_id": binding.installationID, "nonce": binding.nonce, "name": "My scooter",
		"imei": identity.IMEI, "iccid": identity.ICCID, "imsi": identity.IMSI,
	} {
		if announcement[key] != want {
			t.Fatalf("announcement[%s] = %q, want %q", key, announcement[key], want)
		}
	}
	for _, status := range []string{"success", "rollback"} {
		var result map[string]string
		if err := json.Unmarshal(binding.result("txn-1", status, "reason"), &result); err != nil {
			t.Fatal(err)
		}
		if result["installation_id"] != binding.installationID || result["nonce"] != binding.nonce ||
			result["txn_id"] != "txn-1" || result["status"] != status || result["reason"] != "reason" {
			t.Fatalf("unbound result: %v", result)
		}
	}
}

func TestBootstrapRejectsInvalidInstallationBeforeConnecting(t *testing.T) {
	if _, _, err := bootstrapCredential(models.BootstrapConfig{Code: "FGHX7A", InstallationID: "../+/#"}); err == nil {
		t.Fatal("unsafe installation ID accepted")
	}
}
