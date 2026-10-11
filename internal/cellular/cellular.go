// Package cellular configures NetworkManager and the modem's LTE attach context.
package cellular

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"radio-gaga/internal/models"
)

func Validate(c models.CellularConfig) error {
	switch c.Auth {
	case "", "none", "pap", "chap":
	default:
		return fmt.Errorf("cellular.auth must be none, pap, or chap")
	}
	for _, field := range []struct{ name, value string }{{"apn", c.APN}, {"username", c.Username}, {"password", c.Password}} {
		// AT string literals and mmcli's property list have no common escaping.
		if strings.ContainsAny(field.value, "\"'\r\n,\x00") {
			return fmt.Errorf("cellular.%s contains an unsupported quote, comma, or control character", field.name)
		}
	}
	if strings.ContainsAny(c.Connection, "\r\n\x00") {
		return fmt.Errorf("invalid cellular.connection")
	}
	return nil
}

// Run applies a startup snapshot, retrying until the platform is available.
// Librescoot's modem service owns reconciliation after settings are seeded.
func Run(ctx context.Context, r *redis.Client, c models.CellularConfig, librescoot bool) {
	for {
		var err error
		if librescoot {
			err = SeedSettings(ctx, r, c)
		} else {
			err = Apply(ctx, c, runCommand)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			log.Printf("cellular: configuration applied")
			return
		}
		log.Printf("cellular: configuration unavailable: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

var seedScript = redis.NewScript(`
local changed = {}
for i = 1, #ARGV, 2 do
 local current = redis.call('HGET', KEYS[1], ARGV[i])
 if not current or current == '' then
  redis.call('HSET', KEYS[1], ARGV[i], ARGV[i+1])
  table.insert(changed, ARGV[i])
 end
end
for _, field in ipairs(changed) do
 redis.call('PUBLISH', KEYS[1], field)
end
return #changed
`)

// SeedSettings atomically fills missing or empty values without replacing
// platform settings. Notifications contain field names, never credentials.
func SeedSettings(ctx context.Context, r *redis.Client, c models.CellularConfig) error {
	if err := Validate(c); err != nil {
		return err
	}
	auth := c.Auth
	if auth == "" {
		auth = "none"
	}
	return seedScript.Run(ctx, r, []string{"settings"},
		"cellular.apn", c.APN, "cellular.username", c.Username,
		"cellular.password", c.Password, "cellular.auth", auth).Err()
}

type runner func(context.Context, string, ...string) ([]byte, error)

var atError = regexp.MustCompile(`(?m)(?:^|[\r\n'])\s*(?:ERROR|\+CME ERROR:[^\r\n']*|\+CMS ERROR:[^\r\n']*)\s*(?:$|[\r\n'])`)

func sendAT(ctx context.Context, run runner, modem, command string) error {
	out, err := run(ctx, "mmcli", "-m", modem, "--timeout=70", "--command="+command)
	if err != nil {
		return err
	}
	if atError.Match(out) {
		return fmt.Errorf("modem rejected AT command")
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		// Neither argv nor stderr is safe to log: both can contain credentials.
		return nil, fmt.Errorf("%s failed", name)
	}
	return out, nil
}

func Apply(ctx context.Context, c models.CellularConfig, run runner) error {
	if err := Validate(c); err != nil {
		return err
	}
	connection := c.Connection
	if connection == "" {
		connection = "wwan"
	}
	auth := c.Auth
	if auth == "" {
		auth = "none"
	}
	raw, err := run(ctx, "mmcli", "-J", "-L")
	if err != nil {
		return fmt.Errorf("discover modem: %w", err)
	}
	var list struct {
		Modems []string `json:"modem-list"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list.Modems) == 0 {
		return fmt.Errorf("no modem available")
	}
	modem := list.Modems[0]
	if _, err := run(ctx, "nmcli", "connection", "modify", connection,
		"gsm.apn", c.APN, "gsm.username", c.Username, "gsm.password", c.Password,
		"gsm.password-flags", "0", "gsm.auto-config", "no",
		"ppp.refuse-pap", fmt.Sprint(auth != "pap"),
		"ppp.refuse-chap", fmt.Sprint(auth != "chap"),
		"ppp.refuse-eap", "yes", "ppp.refuse-mschap", "yes", "ppp.refuse-mschapv2", "yes"); err != nil {
		return fmt.Errorf("configure data bearer: %w", err)
	}
	// MM's generic EPS setter may be unavailable on older modem firmware.
	// SIM7100E attaches using CID 1, regardless of the generic EPS context.
	_, _ = run(ctx, "mmcli", "-m", modem, "--3gpp-set-initial-eps-bearer-settings="+
		fmt.Sprintf("apn='%s',user='%s',password='%s',allowed-auth=%s,ip-type=ipv4", c.APN, c.Username, c.Password, auth))
	pdp := "IP"
	if c.APN == "" {
		pdp = "IPV4V6"
	}
	at := []string{fmt.Sprintf(`AT+CGDCONT=1,"%s","%s"`, pdp, c.APN)}
	authType := 0
	if auth == "pap" {
		authType = 1
	}
	if auth == "chap" {
		authType = 2
	}
	authentication := "AT+CGAUTH=1,0"
	if authType != 0 && (c.Username != "" || c.Password != "") {
		authentication = fmt.Sprintf(`AT+CGAUTH=1,%d,"%s","%s"`, authType, c.Username, c.Password)
	}
	at = append(at, authentication)
	for _, command := range at {
		if err := sendAT(ctx, run, modem, command); err != nil {
			return fmt.Errorf("configure attach context: %w", err)
		}
	}
	// Re-registration is required for the new attach context to take effect.
	_, _ = run(ctx, "nmcli", "connection", "down", connection)
	for _, command := range []string{"AT+COPS=2", "AT+COPS=0"} {
		if err := sendAT(ctx, run, modem, command); err != nil {
			// Restore the data connection even if re-registration fails.
			_, _ = run(ctx, "nmcli", "connection", "up", connection)
			return fmt.Errorf("reattach: %w", err)
		}
	}
	if _, err := run(ctx, "nmcli", "connection", "up", connection); err != nil {
		return fmt.Errorf("activate data bearer: %w", err)
	}
	return nil
}
