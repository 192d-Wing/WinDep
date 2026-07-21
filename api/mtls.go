package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/gofiber/fiber/v2"
)

// ingestTransport builds the HTTP transport for forwarding telemetry to the admin
// ingest listener. With ADMIN_INGEST_CA set it verifies the admin server cert and (with
// ADMIN_INGEST_CLIENT_CERT/KEY) presents a client cert — true mTLS. Without it, it falls
// back to the legacy skip-verify east-west behavior. Fatal on misconfigured cert paths.
func ingestTransport() *http.Transport {
	ca := os.Getenv("ADMIN_INGEST_CA")
	if ca == "" {
		return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // east-west, network-isolated; opt into mTLS via ADMIN_INGEST_CA
	}
	pool, err := certPool(ca)
	if err != nil {
		slog.Error("ADMIN_INGEST_CA", "err", err.Error())
		os.Exit(1)
	}
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	crt, key := os.Getenv("ADMIN_INGEST_CLIENT_CERT"), os.Getenv("ADMIN_INGEST_CLIENT_KEY")
	if crt != "" && key != "" {
		pair, err := tls.LoadX509KeyPair(crt, key)
		if err != nil {
			slog.Error("ingest client cert", "err", err.Error())
			os.Exit(1)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return &http.Transport{TLSClientConfig: cfg}
}

// serve starts app on addr. With cert+key it serves TLS (mTLS — verifying client certs
// — when clientCA is also set); otherwise plaintext, expecting upstream termination.
// It blocks until the listener stops.
func serve(app *fiber.App, addr, cert, key, clientCA string) error {
	if cert == "" || key == "" {
		slog.Warn("listening WITHOUT TLS (expecting TLS termination upstream)", "addr", addr)
		return app.Listen(addr)
	}
	cfg, err := serverTLSConfig(cert, key, clientCA, false)
	if err != nil {
		return err
	}
	ln, err := tlsListener(addr, cfg)
	if err != nil {
		return err
	}
	if clientCA != "" {
		slog.Info("listening with mTLS", "addr", addr)
	} else {
		slog.Info("listening with TLS", "addr", addr)
	}
	return app.Listener(ln)
}

// serverTLSConfig builds a server TLS config presenting cert/key. When clientCAFile is
// set, client certs are verified against it (mTLS). require=false uses
// VerifyClientCertIfGiven so certless health/metrics probes still complete the
// handshake (per-route enforcement is done by requireClientCert); require=true mandates
// a client cert on every connection to the listener.
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

// requireClientCert rejects requests that did not present a verified client
// certificate. It is applied to the /api group when CLIENT_CA is configured, so the
// health/metrics endpoints (registered outside the group) stay reachable by certless
// kubelet/Prometheus probes while /api is mutually authenticated.
func requireClientCert(c *fiber.Ctx) error {
	if st := c.Context().TLSConnectionState(); st != nil && len(st.VerifiedChains) > 0 {
		return c.Next()
	}
	return fiber.NewError(fiber.StatusForbidden, "client certificate required")
}
