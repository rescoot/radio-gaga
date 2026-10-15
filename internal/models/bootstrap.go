package models

import (
	"fmt"
	"regexp"
)

var installationIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (b BootstrapConfig) ValidateInstallationID() error {
	if b.InstallationID != "" && (!installationIDPattern.MatchString(b.InstallationID) ||
		b.InstallationID == "00000000-0000-0000-0000-000000000000") {
		return fmt.Errorf("bootstrap.installation_id must be a nonzero lowercase UUID")
	}
	return nil
}
