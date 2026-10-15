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
	s3Server        *http.Server
	consoleServer   *http.Server
	s3Router        *s3api.Router
	engine          storage.Engine
	multipartExpiry time.Duration

	s3Listener      net.Listener
	consoleListener net.Listener

	// errCh carries a listener's failure after startup to main. Buffered so
	// neither serving goroutine blocks on a reader that is gone.
	errCh chan error

	// cleanupCancel stops the background cleanup worker; cleanupDone is
	// closed once it has returned. Both are set by Start.
	cleanupCancel context.CancelFunc
	cleanupDone   chan struct{}

	shutdownOnce sync.Once
	shutdownErr  error
}

// defaultMultipartExpiry applies when the config leaves it unset (as configs
// built directly, rather than through config.Load, do).
const defaultMultipartExpiry = 7 * 24 * time.Hour

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

	creds := auth.NewCredentials(cfg.AccessKey, cfg.SecretKey)

	// S3 API server
	s3Router := s3api.NewRouter(s3api.RouterOptions{
		Engine:           engine,
		Creds:            creds,
		Region:           cfg.Region,
		Domain:           cfg.Domain,
		TrustProxy:       cfg.TrustProxy,
		TrustedProxyHops: cfg.TrustedProxyHops,
		Metrics:          engine.Metrics(),
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
		AllowedOrigins:   cfg.ConsoleAllowedOrigins,
		JWTSecret:        jwtSecret,
		Metrics:          engine.Metrics(),
		DataDir:          cfg.DataDir,
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

	multipartExpiry := cfg.MultipartExpiry
	if multipartExpiry <= 0 {
		multipartExpiry = defaultMultipartExpiry
	}

	return &Server{
		s3Server:        s3Server,
		consoleServer:   consoleServer,
		s3Router:        s3Router,
		engine:          engine,
		multipartExpiry: multipartExpiry,
		errCh:           make(chan error, 2),
	}, nil
}

// Start launches both servers concurrently and starts background workers.
// A server that fails after this returns is reported on Errors().
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
	s.s3Listener, s.consoleListener = s3Listener, consoleListener

	go s.serve("S3 API", s.s3Server, s3Listener)
	go s.serve("Console", s.consoleServer, consoleListener)

	cleanupCtx, cancel := context.WithCancel(context.Background())
	s.cleanupCancel = cancel
	s.cleanupDone = make(chan struct{})
	go s.runCleanup(cleanupCtx)

	return nil
}

// Errors reports a server that stopped serving on its own after Start. It
// used to be logged and otherwise ignored, leaving the process running with
// one of its two servers dead.
func (s *Server) Errors() <-chan error {
	return s.errCh
}

func (s *Server) serve(name string, srv *http.Server, l net.Listener) {
	var err error
	if srv.TLSConfig != nil {
		slog.Info("["+name+"] Listening securely (HTTPS)", "addr", srv.Addr)
		err = srv.ServeTLS(l, "", "")
	} else {
		slog.Info("["+name+"] Listening", "addr", srv.Addr)
		err = srv.Serve(l)
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return
	}
	slog.Error("["+name+"] Server error", "error", err)
	select {
	case s.errCh <- fmt.Errorf("%s server stopped: %w", name, err):
	default: // the other server already reported; one error is enough
	}
}

// runCleanup runs the multipart and lifecycle sweeps at startup and then
// hourly, until ctx is cancelled. The sweeps check ctx between items, so
// cancellation also cuts a running sweep short.
func (s *Server) runCleanup(ctx context.Context) {
	defer close(s.cleanupDone)
	slog.Info("[Server] Starting background multipart cleanup worker", "multipartExpiry", s.multipartExpiry)

	sweep := func() {
		if err := s.engine.CleanExpiredMultipartUploads(ctx, s.multipartExpiry); err != nil && ctx.Err() == nil {
			slog.Error("[Server] Error cleaning expired multipart uploads", "error", err)
		}
		if err := s.engine.CleanExpiredObjects(ctx); err != nil && ctx.Err() == nil {
			slog.Error("[Server] Error cleaning expired objects", "error", err)
		}
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	sweep()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-ctx.Done():
			slog.Info("[Server] Background cleanup worker stopped")
			return
		}
	}
}

// Shutdown gracefully shuts down both servers and stops background workers,
// giving up on whatever is still running when ctx expires.
//
// It is safe to call more than once — a signal handler racing with a manual
// admin shutdown call, or an orchestrator retrying after a timeout, could
// otherwise call it twice. Only the first call does any work; later calls
// return the same result.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.shutdown(ctx)
	})
	return s.shutdownErr
}

func (s *Server) shutdown(ctx context.Context) error {
	slog.Info("[Server] Shutting down")

	var errs []error

	// Stop the cleanup worker first and wait for it: it uses the engine,
	// which is closed last. It used to be signalled and not waited for.
	if s.cleanupCancel != nil {
		s.cleanupCancel()
		select {
		case <-s.cleanupDone:
		case <-ctx.Done():
			errs = append(errs, fmt.Errorf("cleanup worker still running at the shutdown deadline: %w", ctx.Err()))
		}
	}

	if err := s.s3Server.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("s3 API server shutdown error: %w", err))
	}
	if err := s.s3Router.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}

	if err := s.consoleServer.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("console server shutdown error: %w", err))
	}

	// Bounded by the same ctx: past it, undelivered replication and webhook
	// events are abandoned (and counted) instead of holding the process open.
	if err := s.engine.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("storage engine shutdown error: %w", err))
	}

	return errors.Join(errs...)
}
