// Package ca manages the TLS-interception CA shared by every nono proxy.
// The private key only ever goes to proxy containers; sandboxes get the cert.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type CA struct {
	CertPEM []byte
	KeyPEM  []byte
}

// LoadOrCreate reads ca.crt / ca.key from dir, generating them when missing or
// close to expiry. nono requires an ECDSA P-256 key in PKCS#8 PEM.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	c, errC := os.ReadFile(certPath)
	k, errK := os.ReadFile(keyPath)
	if errC == nil && errK == nil && valid(c) {
		return &CA{CertPEM: c, KeyPEM: k}, nil
	}
	if errC != nil && !errors.Is(errC, os.ErrNotExist) {
		return nil, errC
	}
	ca, err := generate()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, ca.KeyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		return nil, err
	}
	return ca, nil
}

func valid(certPEM []byte) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return false
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	return err == nil && time.Until(cert.NotAfter) > 30*24*time.Hour
}

func generate() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "remodev egress proxy CA", Organization: []string{"remodev"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &CA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}),
	}, nil
}
