package ingress

import (
	"crypto/x509"
	"encoding/pem"
	"time"
)

// leafNotAfter returns the expiry of the first certificate in a PEM bundle.
func leafNotAfter(pemBytes []byte) (time.Time, bool) {
	for {
		var b *pem.Block
		b, pemBytes = pem.Decode(pemBytes)
		if b == nil {
			return time.Time{}, false
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return time.Time{}, false
		}
		return c.NotAfter, true
	}
}
