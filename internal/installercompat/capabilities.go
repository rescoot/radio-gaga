package installercompat

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

const Report = `{"enrollment_protocol":1,"installation_binding":1,"installer_protocol":1,"bootstrap_announce":1,"mqtt_username":1,"verified_tls":1,"offline_config":1}`

func Check(ctx context.Context, binary, config string) error {
	output, err := exec.CommandContext(ctx, binary, "-bootstrap-capabilities", "-config", config).Output()
	if err != nil {
		return fmt.Errorf("installer capability check failed: %w", err)
	}
	var got, required map[string]int
	if json.Unmarshal(output, &got) != nil {
		return fmt.Errorf("invalid installer capability report")
	}
	if err := json.Unmarshal([]byte(Report), &required); err != nil {
		return err
	}
	for name, version := range required {
		if got[name] != version {
			return fmt.Errorf("missing installer capability: %s", name)
		}
	}
	return nil
}
