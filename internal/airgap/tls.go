package airgap

import "crypto/tls"

func tlsSkipVerify() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec
}
