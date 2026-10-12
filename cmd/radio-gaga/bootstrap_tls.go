package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"

	"radio-gaga/internal/models"
)

func bootstrapTLSConfig(config models.MQTTConfig) (*tls.Config, error) {
	broker, err := url.Parse(config.BrokerURL)
	if err != nil || broker == nil || (broker.Scheme != "ssl" && broker.Scheme != "tls") ||
		broker.Hostname() == "" || broker.User != nil || broker.Path != "" ||
		broker.RawQuery != "" || broker.Fragment != "" {
		return nil, fmt.Errorf("bootstrap broker must be a TLS endpoint without credentials or a path")
	}

	result := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: broker.Hostname()}
	var ca []byte
	switch {
	case config.CACertEmbedded != "":
		ca = []byte(config.CACertEmbedded)
	case config.CACert != "":
		ca, err = os.ReadFile(config.CACert)
		if err != nil {
			return nil, fmt.Errorf("read bootstrap CA: %w", err)
		}
	default:
		return result, nil
	}

	result.RootCAs = x509.NewCertPool()
	if !result.RootCAs.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("bootstrap CA contains no certificates")
	}
	return result, nil
}
