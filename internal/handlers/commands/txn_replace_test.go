package commands

import (
	"testing"

	"radio-gaga/internal/txn"
)

// The restart decision is the part that has bitten twice: A2 recorded that
// kind=config commits without restarting, which means a pushed config silently
// does not take effect until the next unrelated restart. Cover the matrix.
func TestRestartNeeded(t *testing.T) {
	cases := []struct {
		name      string
		kind      txn.Kind
		requested bool
		want      bool
	}{
		{"config alone does not restart", txn.KindConfig, false, false},
		{"config with the opt-in restarts", txn.KindConfig, true, true},
		{"binary always restarts", txn.KindBinary, false, true},
		{"binary with the opt-in restarts", txn.KindBinary, true, true},
		{"both always restarts", txn.KindBoth, false, true},
		{"both with the opt-in restarts", txn.KindBoth, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restartNeeded(tc.kind, tc.requested); got != tc.want {
				t.Fatalf("restartNeeded(%q, %v) = %v, want %v", tc.kind, tc.requested, got, tc.want)
			}
		})
	}
}
