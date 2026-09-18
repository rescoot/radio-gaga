package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The username is a cross-language contract: the device hashes the code, and
// Sunshine hashes the digest of the same code. These vectors are mirrored in
// test/models/bootstrap_token_test.rb, so a change to either normalization or
// the derivation on one side fails the other side's tests.
func TestBootstrapBrokerUsername(t *testing.T) {
	cases := []struct {
		name string
		code string
		want string
	}{
		// sha256("FGHX7A")[0,16]
		{"canonical code", "FGHX7A", "186856c4dd2e60a0"},
		// sha256("FG0X7A")[0,16] — the code the misread forms below must reach.
		{"code containing a zero", "FG0X7A", "ddf0e5f01be7390a"},
		// Lowercase, surrounding whitespace, a hyphen from a copied URL, and an
		// O read for a 0 must all land on the credential the server created.
		{"lowercase", "fghx7a", "186856c4dd2e60a0"},
		{"padded", "  FGHX7A  ", "186856c4dd2e60a0"},
		{"hyphenated from a copied url", "FGH-X7A", "186856c4dd2e60a0"},
		{"letter o for zero", "FGOX7A", "ddf0e5f01be7390a"},
		// I and L both read as 1, and sha256("FG1X7A")[0,16] is below.
		{"code containing a one", "FG1X7A", "0ef5d0f4c7920646"},
		{"letter i for one", "FGIX7A", "0ef5d0f4c7920646"},
		{"letter l for one", "FGLX7A", "0ef5d0f4c7920646"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootstrapBrokerUsername(tc.code); got != tc.want {
				t.Fatalf("bootstrapBrokerUsername(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// Mirrors BootstrapToken.normalize_short_code's own test vectors.
func TestNormalizeShortCode(t *testing.T) {
	cases := map[string]string{
		"ab-o1u z": "AB01VZ",
		"Il1U":     "111V",
		"FG0X7A":   "FG0X7A",
		// U is not in the generator's alphabet; a typed U becomes V, which is why
		// "FGUHG1" is not a code any user could have been given.
		"FGUHG1": "FGVHG1",
		"":       "",
	}

	for input, want := range cases {
		if got := normalizeShortCode(input); got != want {
			t.Fatalf("normalizeShortCode(%q) = %q, want %q", input, got, want)
		}
	}
}

// The username must not contain the code, or it would land in every broker log
// line: mosquitto logs the username but not the password.
func TestBrokerUsernameDoesNotLeakTheCode(t *testing.T) {
	const code = "FGHX7A"

	username := bootstrapBrokerUsername(code)
	if len(username) != 16 {
		t.Fatalf("username %q should be a 16-character digest prefix", username)
	}
	if username == code || len(username) < len(code) {
		t.Fatalf("the username must be a digest, not the code")
	}
}

func TestAnnouncePayloadOmitsEmptyIdentifiers(t *testing.T) {
	payload := announcePayload("866802022030847", "", "", "v0.3.44", "librescoot", "", "nonce1")

	var decoded map[string]string
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	if decoded["imei"] != "866802022030847" {
		t.Fatalf("imei missing from payload: %v", decoded)
	}
	if _, present := decoded["mdb_serial"]; present {
		t.Fatalf("an empty identifier must be omitted, not sent as an empty string: %v", decoded)
	}
	if decoded["nonce"] != "nonce1" {
		t.Fatalf("nonce missing: %v", decoded)
	}
	if decoded["platform"] != "librescoot" {
		t.Fatalf("platform missing: %v", decoded)
	}
}

func TestApplyPushedConfigRejectsWhatItCannotApply(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	cases := []struct {
		name    string
		payload string
	}{
		{"not JSON", "not json"},
		{"not a txn replace", `{"command":"redis","params":{}}`},
		{"no config", `{"command":"txn:replace","params":{"kind":"config"}}`},
		{"unsupported kind", `{"command":"txn:replace","params":{"kind":"binary","config_yaml":"a: b"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			committed, err := applyPushedConfig([]byte(tc.payload), configPath)
			if err == nil {
				t.Fatalf("expected an error, got committed=%v", committed)
			}
			if committed {
				t.Fatalf("a rejected payload must not report a commit")
			}
		})
	}
}
