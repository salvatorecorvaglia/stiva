package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/config"
)

// freePort finds an available TCP port by briefly binding to it, matching
// the pattern TestServerStartPortConflict already uses below.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestServerStartSuccessAndShutdown(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		AccessKey:   "testkey",
		SecretKey:   "testsecret",
		DataDir:     tempDir,
		S3Port:      0, // OS will assign an ephemeral port
		ConsolePort: 0, // OS will assign an ephemeral port
		Region:      "us-east-1",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Clean up and shutdown
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown returned error: %v", err)
	}
}

// TestServerShutdownIsIdempotent guards against a panic ("close of closed
// channel") when Shutdown is called more than once — plausible under a
// signal handler racing with a manual admin shutdown call, or an
// orchestrator retrying after a timeout.
func TestServerShutdownIsIdempotent(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		AccessKey:   "testkey",
		SecretKey:   "testsecret",
		DataDir:     tempDir,
		S3Port:      0,
		ConsolePort: 0,
		Region:      "us-east-1",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown returned error: %v", err)
	}
	// Must not panic.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown returned error: %v", err)
	}
}

// TestServerTLSStartupAcceptsHandshake covers the TLS branch in New()/Start()
// — cfg.TLSEnabled with no cert/key files provided, which generates a
// self-signed certificate in memory (GenerateSelfSignedCert) and serves both
// listeners with ServeTLS. This path previously had no test coverage at all,
// unlike the plaintext path.
func TestServerTLSStartupAcceptsHandshake(t *testing.T) {
	tempDir := t.TempDir()
	s3Port := freePort(t)
	consolePort := freePort(t)

	cfg := &config.Config{
		AccessKey:   "testkey",
		SecretKey:   "testsecret",
		DataDir:     tempDir,
		S3Port:      s3Port,
		ConsolePort: consolePort,
		Region:      "us-east-1",
		TLSEnabled:  true, // no TLSCert/TLSKey: self-signed cert generated in memory
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer srv.Shutdown(context.Background())

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed
		},
		Timeout: 5 * time.Second,
	}

	// Reaching this point at all — regardless of status code — proves the
	// TLS handshake succeeded and the HTTP server behind it responded.
	for name, port := range map[string]int{"S3 API": s3Port, "Console": consolePort} {
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatalf("HTTPS request to %s (port %d) failed: %v", name, port, err)
		}
		resp.Body.Close()
	}
}

func TestServerStartPortConflict(t *testing.T) {
	tempDir := t.TempDir()

	// Bind a port manually to simulate conflict on the wildcard interface
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to bind test listener: %v", err)
	}
	defer l.Close()

	_, portStr, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("Failed to parse listener port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Failed to convert port: %v", err)
	}

	// Create config with conflicting S3Port
	cfg := &config.Config{
		AccessKey:   "testkey",
		SecretKey:   "testsecret",
		DataDir:     tempDir,
		S3Port:      port,
		ConsolePort: 0,
		Region:      "us-east-1",
	}

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to create server: %v", err)
	}
	defer srv.Shutdown(context.Background())

	// Try starting the server - it should fail synchronously
	err = srv.Start()
	if err == nil {
		t.Error("Expected error when starting server with conflicting port, got nil")
	}
}
