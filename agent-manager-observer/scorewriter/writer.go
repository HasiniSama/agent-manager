// Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package scorewriter is the score writer, the only service that writes the score database.
package scorewriter

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
	"github.com/wso2/agent-manager/agent-manager-observer/middleware/logger"
)

const readyTimeout = 2 * time.Second

// Writer serves the publish and internal listeners.
type Writer struct {
	publish   *http.Server
	internal  *http.Server
	pool      *pgxpool.Pool
	store     scoreStore
	runTokens *runTokenVerifier
	retention time.Duration
	now       func() time.Time
}

// New builds the writer; it fails when the TLS key pair or the database files can't be read.
func New(cfg *config.ScoreWriterConfig) (*Writer, error) {
	tlsCfg, err := serverTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	pool, err := newPool(cfg.Database)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		pool:      pool,
		store:     &store{pool: pool},
		runTokens: newRunTokenVerifier(cfg.RunToken, &http.Client{Timeout: jwksFetchTimeout}),
		retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		now:       time.Now,
	}
	w.publish = newServer(tlsCfg, w.publishHandler())
	w.internal = newServer(tlsCfg, w.internalHandler())
	return w, nil
}

// Serve serves both listeners until one fails or Shutdown is called.
func (w *Writer) Serve(publishLn, internalLn net.Listener) error {
	errc := make(chan error, 2)
	go func() { errc <- serveTLS(w.publish, publishLn, "publish") }()
	go func() { errc <- serveTLS(w.internal, internalLn, "internal") }()
	return <-errc
}

// Shutdown stops both listeners gracefully and closes the database pool.
func (w *Writer) Shutdown(ctx context.Context) error {
	err := errors.Join(w.publish.Shutdown(ctx), w.internal.Shutdown(ctx))
	w.pool.Close()
	return err
}

// Run serves the writer until SIGINT or SIGTERM, then shuts down gracefully.
func Run(cfg *config.ScoreWriterConfig) error {
	setupLogger(cfg.LogLevel)

	w, err := New(cfg)
	if err != nil {
		return err
	}
	publishLn, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.PublishPort))
	if err != nil {
		w.pool.Close()
		return fmt.Errorf("listen on the publish port: %w", err)
	}
	internalLn, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.InternalPort))
	if err != nil {
		_ = publishLn.Close()
		w.pool.Close()
		return fmt.Errorf("listen on the internal port: %w", err)
	}
	slog.Info("Score writer listening", "publishPort", cfg.PublishPort, "internalPort", cfg.InternalPort)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- w.Serve(publishLn, internalLn) }()

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		slog.Info("Shutting down score writer")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := w.Shutdown(shutdownCtx); shutdownErr != nil {
		err = errors.Join(err, shutdownErr)
	}
	if err != nil {
		return fmt.Errorf("score writer stopped: %w", err)
	}
	slog.Info("Score writer exited")
	return nil
}

// publishHandler serves the publish port's one route.
func (w *Writer) publishHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(publishRoute, w.publishScores)
	return logger.RequestLogger()(mux)
}

// internalHandler serves the internal port's probes.
func (w *Writer) internalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		writeStatus(rw, http.StatusOK, "healthy")
	})
	mux.HandleFunc("GET /readyz", w.readyz)
	return mux
}

// readyz answers 200 once the writer login reaches the database.
func (w *Writer) readyz(rw http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := w.pool.Ping(ctx); err != nil {
		slog.Warn("Score database not reachable", "error", err)
		writeStatus(rw, http.StatusServiceUnavailable, "unavailable")
		return
	}
	writeStatus(rw, http.StatusOK, "ready")
}

func writeStatus(rw http.ResponseWriter, code int, status string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(code)
	_, _ = fmt.Fprintf(rw, `{"status":%q}`, status)
}

// serverTLSConfig loads the listener key pair: TLS 1.2 or later, AEAD ciphers with forward secrecy.
func serverTLSConfig(files config.TLSFiles) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load the TLS key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}, nil
}

func newServer(tlsCfg *tls.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
}

func serveTLS(srv *http.Server, ln net.Listener, name string) error {
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

func setupLogger(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})))
}
