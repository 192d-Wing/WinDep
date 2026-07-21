package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
)

// serverTLSConfig builds a server TLS config presenting cert/key. When clientCAFile is
// set, client certs are verified against it. require=true mandates a client cert on
// every connection (used by the dedicated mTLS ingest listener); require=false uses
// VerifyClientCertIfGiven.
func serverTLSConfig(certFile, keyFile, clientCAFile string, require bool) (*tls.Config, error) {
	crt, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{crt}, MinVersion: tls.VersionTLS12}
	if clientCAFile != "" {
		pool, err := certPool(clientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		if require {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		} else {
			cfg.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	return cfg, nil
}

// certPool reads a PEM bundle into an x509 pool.
func certPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates parsed from %s", caFile)
	}
	return pool, nil
}

// tlsListener wraps a TCP listener on addr with cfg.
func tlsListener(addr string, cfg *tls.Config) (net.Listener, error) {
	return tls.Listen("tcp", addr, cfg)
}
