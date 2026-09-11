// Package server manages the HTTP servers for the S3 API and web console.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/config"
	"github.com/salvatorecorvaglia/stiva/internal/console"
	"github.com/salvatorecorvaglia/stiva/internal/s3api"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// Server holds both the S3 API server and the web console server, plus background workers.
type Server struct {
	s3Server      *http.Server
	consoleServer *http.Server
	engine        storage.Engine
	cleanupStop   chan struct{}
	shutdownOnce  sync.Once
}

// New creates and configures both servers.
func New(cfg *config.Config) (*Server, error) {
	var syncCfg *storage.SyncConfig
	if cfg.SyncEndpoint != "" {
		syncCfg = &storage.SyncConfig{
			Endpoint:  strings.TrimSuffix(cfg.SyncEndpoint, "/"),
			Bucket:    cfg.SyncBucket,
			AccessKey: cfg.SyncAccessKey,
			SecretKey: cfg.SyncSecretKey,
			Region:    cfg.SyncRegion,
		}
	}

	// Initialize storage engine
	engine, err := storage.NewFilesystemEngine(cfg.DataDir, syncCfg, cfg.WebhookURL)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	engine.SetWebhookSecret(cfg.WebhookSecret)
	engine.SetMaxObjectSize(cfg.MaxObjectSize)
	engine.SetDisableMinPartSize(cfg.DisableMinPartSize)

	// Set temporary directory for auth hashing
	auth.TempDir = filepath.Join(cfg.DataDir, "tmp")
	if err := os.MkdirAll(auth.TempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temporary hashing directory: %w", err)
	}

	// Setup persistent JWT secret for Console session tracking
	var jwtSecret []byte
	if cfg.JWTSecret != "" {
		jwtSecret = []byte(cfg.JWTSecret)
	} else {
		slog.Warn("STIVA_JWT_SECRET environment variable is not set. A random secret will be generated. For persistent console sessions across restarts, please set STIVA_JWT_SECRET.")
		storedSecret, err := engine.GetSystemValue("jwt_secret")
		if err != nil {
			// A real I/O error, not "not set" — GetSystemValue returns
			// ("", nil) when the key is simply absent. Proceeding silently
			// here previously masked genuine storage problems.
			slog.Error("[Server] Failed to read persisted JWT secret; a new one will be generated for this run", "error", err)
		} else if storedSecret != "" {
			decoded, decodeErr := hex.DecodeString(storedSecret)
			if decodeErr != nil {
				// hex.DecodeString returns whatever it decoded before the
				// error too; discarding it here (rather than keeping a
				// truncated key) matters because a truncated-but-non-empty
				// secret would otherwise pass the len==0 check below and be
				// used anyway, silently invalidating every session with no
				// diagnostic.
				slog.Error("[Server] Persisted JWT secret is corrupted; a new one will be generated and all existing console sessions will be invalidated", "error", decodeErr)
			} else {
				jwtSecret = decoded
			}
		}
		if len(jwtSecret) == 0 {
			jwtSecret = make([]byte, 32)
			if _, err := rand.Read(jwtSecret); err != nil {
				return nil, fmt.Errorf("failed to generate random JWT secret: %w", err)
			}
			if err := engine.PutSystemValue("jwt_secret", hex.EncodeToString(jwtSecret)); err != nil {
				slog.Error("[Server] Failed to persist JWT secret; console sessions will not survive a restart", "error", err)
			}
		}
	}
	console.SetJWTSecret(jwtSecret)

	creds := auth.NewCredentials(cfg.AccessKey, cfg.SecretKey)

	// S3 API server
	s3Router := s3api.NewRouter(s3api.RouterOptions{
		Engine:           engine,
		Creds:            creds,
		Region:           cfg.Region,
		Domain:           cfg.Domain,
		TrustProxy:       cfg.TrustProxy,
		TrustedProxyHops: cfg.TrustedProxyHops,
	})
	s3Server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.S3Port),
		Handler:           s3Router,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Web console server
	consoleHandler := console.NewHandler(console.Options{
		Engine:           engine,
		Creds:            creds,
		S3Port:           cfg.S3Port,
		Region:           cfg.Region,
		S3Endpoint:       cfg.S3Endpoint,
		LoginRateLimit:   cfg.LoginRateLimit,
		APIRateLimit:     cfg.APIRateLimit,
		TrustProxy:       cfg.TrustProxy,
		TrustedProxyHops: cfg.TrustedProxyHops,
		MetricsToken:     cfg.MetricsToken,
	})
	consoleServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.ConsolePort),
		Handler:           consoleHandler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Setup TLS if enabled
	if cfg.TLSEnabled {
		var cert tls.Certificate
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			cert, err = tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
			if err != nil {
				return nil, fmt.Errorf("failed to load TLS certificate and key: %w", err)
			}
		} else {
			slog.Warn("[Server] STIVA_TLS_ENABLED=true but no cert/key files provided. Generating self-signed certificate in memory...")
			cert, err = GenerateSelfSignedCert()
			if err != nil {
				return nil, fmt.Errorf("failed to generate self-signed certificate: %w", err)
			}
		}

		// Each server gets its own *tls.Config, even though the contents are
		// identical: ServeTLS runs concurrently for both servers (below),
		// and each internally mutates its Server.TLSConfig in place (e.g.
		// http2's onceSetNextProtoDefaults appending to NextProtos). A
		// shared pointer here is a genuine data race between those two
		// goroutines, not just a style preference.
		s3Server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		consoleServer.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	return &Server{
		s3Server:      s3Server,
		consoleServer: consoleServer,
		engine:        engine,
		cleanupStop:   make(chan struct{}),
	}, nil
}

// Start launches both servers concurrently and starts background workers.
func (s *Server) Start() error {
	var lc net.ListenConfig
	ctx := context.Background()

	s3Listener, err := lc.Listen(ctx, "tcp", s.s3Server.Addr)
	if err != nil {
		return fmt.Errorf("failed to bind S3 API port %s: %w", s.s3Server.Addr, err)
	}

	consoleListener, err := lc.Listen(ctx, "tcp", s.consoleServer.Addr)
	if err != nil {
		s3Listener.Close()
		return fmt.Errorf("failed to bind Console port %s: %w", s.consoleServer.Addr, err)
	}

	go func() {
		if s.s3Server.TLSConfig != nil {
			slog.Info("[S3 API] Listening securely (HTTPS)", "addr", s.s3Server.Addr)
			if err := s.s3Server.ServeTLS(s3Listener, "", ""); err != nil && err != http.ErrServerClosed {
				slog.Error("[S3 API] Server error", "error", err)
			}
		} else {
			slog.Info("[S3 API] Listening", "addr", s.s3Server.Addr)
			if err := s.s3Server.Serve(s3Listener); err != nil && err != http.ErrServerClosed {
				slog.Error("[S3 API] Server error", "error", err)
			}
		}
	}()

	go func() {
		if s.consoleServer.TLSConfig != nil {
			slog.Info("[Console] Listening securely (HTTPS)", "addr", s.consoleServer.Addr)
			if err := s.consoleServer.ServeTLS(consoleListener, "", ""); err != nil && err != http.ErrServerClosed {
				slog.Error("[Console] Server error", "error", err)
			}
		} else {
			slog.Info("[Console] Listening", "addr", s.consoleServer.Addr)
			if err := s.consoleServer.Serve(consoleListener); err != nil && err != http.ErrServerClosed {
				slog.Error("[Console] Server error", "error", err)
			}
		}
	}()

	// Start background multipart cleanup task
	go func() {
		slog.Info("[Server] Starting background multipart cleanup worker")
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		if err := s.engine.CleanExpiredMultipartUploads(24 * time.Hour); err != nil {
			slog.Error("[Server] Error in initial multipart cleanup", "error", err)
		}
		if err := s.engine.CleanExpiredObjects(); err != nil {
			slog.Error("[Server] Error in initial lifecycle cleanup", "error", err)
		}

		for {
			select {
			case <-ticker.C:
				if err := s.engine.CleanExpiredMultipartUploads(24 * time.Hour); err != nil {
					slog.Error("[Server] Error cleaning expired multipart uploads", "error", err)
				}
				if err := s.engine.CleanExpiredObjects(); err != nil {
					slog.Error("[Server] Error cleaning expired objects", "error", err)
				}
			case <-s.cleanupStop:
				slog.Info("[Server] Background cleanup worker stopped")
				return
			}
		}
	}()

	return nil
}

// Shutdown gracefully shuts down both servers and stops background workers.
// It is safe to call more than once — a signal handler racing with a manual
// admin shutdown call, or an orchestrator retrying after a timeout, could
// otherwise call this twice and panic closing cleanupStop a second time.
// Only the first call does any work; later calls return the same result.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.shutdownOnce.Do(func() {
		err = s.shutdown(ctx)
	})
	return err
}

func (s *Server) shutdown(ctx context.Context) error {
	slog.Info("[Server] Shutting down")

	// Stop background workers
	close(s.cleanupStop)

	var errs []error

	if err := s.s3Server.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("s3 API server shutdown error: %w", err))
	}

	if r, ok := s.s3Server.Handler.(*s3api.Router); ok {
		r.Close()
	}

	if err := s.consoleServer.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("console server shutdown error: %w", err))
	}

	if err := s.engine.Close(); err != nil {
		errs = append(errs, fmt.Errorf("storage engine close error: %w", err))
	}

	return errors.Join(errs...)
}
