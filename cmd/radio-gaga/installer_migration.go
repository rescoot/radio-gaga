package main

import (
	"fmt"
	"os"

	"radio-gaga/internal/config"
)

func prepareInstallerConfig(source, destination string) error {
	data, err := config.PrepareInstallerConfig(source)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create staged configuration: %w", err)
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(destination)
		}
	}()
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("write staged configuration: %w", err)
	}
	if err = file.Close(); err != nil {
		return err
	}
	cfg, err := config.ReadInstallerConfig(destination)
	if err == nil {
		_, err = bootstrapTLSConfig(cfg.MQTT)
	}
	if err != nil {
		return err
	}
	ok = true
	return nil
}
