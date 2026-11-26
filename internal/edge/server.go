package edge

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"

	"github.com/LuneServer/lune/internal/config"
)

// Server manages Lune's embedded HTTP/HTTPS edge listener.
type Server struct {
	cfg     config.Config
	handler http.Handler
	logger  *slog.Logger
}

// New creates a new embedded edge server.
func New(cfg config.Config, handler http.Handler, logger *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		handler: handler,
		logger:  logger,
	}
}

// Run starts the edge server and shuts it down gracefully when ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	switch s.cfg.EdgeMode {
	case config.EdgeModeLocal, config.EdgeModeDev, config.EdgeModeProxy, config.EdgeModeTailscale:
		return s.runHTTP(ctx)
	case config.EdgeModeACME:
		return s.runACME(ctx)
	default:
		return fmt.Errorf("unsupported edge mode: %s", s.cfg.EdgeMode)
	}
}

func (s *Server) runHTTP(ctx context.Context) error {
	rawLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	ln := newConnLimitListener(rawLn, defaultMaxConnsGlobal, defaultMaxConnsPerIP)

	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// WriteTimeout is intentionally 0 so long-lived media streams and SSE are not cut off.
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("lune edge listening (HTTP)", "mode", s.cfg.EdgeMode, "addr", s.cfg.ListenAddr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// hostEnforcementMiddleware rejects requests whose Host header does not match the
// configured ACME domain, defending against DNS rebinding and direct-IP probing.
func hostEnforcementMiddleware(domain string, next http.Handler) http.Handler {
	expected := strings.ToLower(strings.TrimSpace(domain))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !strings.EqualFold(strings.TrimSpace(host), expected) {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) runACME(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.CertDir(), 0o700); err != nil {
		return fmt.Errorf("create cert directory: %w", err)
	}

	storage := &certmagic.FileStorage{Path: s.cfg.CertDir()}
	certmagic.Default.Storage = storage
	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = s.cfg.ACMEEmail
	if s.cfg.ACMEStaging {
		certmagic.DefaultACME.CA = certmagic.LetsEncryptStagingCA
	} else {
		certmagic.DefaultACME.CA = certmagic.LetsEncryptProductionCA
	}

	if s.cfg.ACMEDNSProvider == "cloudflare" {
		certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: &cloudflare.Provider{APIToken: s.cfg.CloudflareAPIToken},
			},
		}
		s.logger.Info("configured ACME DNS-01 solver", "provider", "cloudflare")
	}

	magic := certmagic.NewDefault()
	if err := magic.ManageSync(ctx, []string{s.cfg.Domain}); err != nil {
		return fmt.Errorf("manage ACME certificate for %s: %w", s.cfg.Domain, err)
	}

	tlsCfg := magic.TLSConfig()
	tlsCfg.MinVersion = tls.VersionTLS12
	// Pin AEAD-only cipher suites for TLS 1.2 (TLS 1.3 suites are always AEAD and managed by Go).
	tlsCfg.CipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)

	rawHTTPSLn, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen HTTPS %s: %w", s.cfg.ListenAddr, err)
	}
	httpsLn := tls.NewListener(newConnLimitListener(rawHTTPSLn, defaultMaxConnsGlobal, defaultMaxConnsPerIP), tlsCfg)

	httpsSrv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           hostEnforcementMiddleware(s.cfg.Domain, s.handler),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	// HTTP-01 challenge + HTTPS redirect listener on :80.
	// When using DNS-01, port 80 is optional so users only need to expose :443.
	var httpSrv *http.Server
	var httpLn net.Listener
	if rawHTTPLn, err := net.Listen("tcp", s.cfg.HTTPAddr); err != nil {
		if s.cfg.ACMEDNSProvider == "" {
			_ = httpsLn.Close()
			return fmt.Errorf("listen HTTP %s: %w", s.cfg.HTTPAddr, err)
		}
		s.logger.Warn("HTTP redirect port unavailable under DNS-01; serving HTTPS only", "addr", s.cfg.HTTPAddr, "error", err)
	} else {
		httpLn = newConnLimitListener(rawHTTPLn, defaultMaxConnsGlobal, defaultMaxConnsPerIP)
		httpSrv = &http.Server{
			Addr: s.cfg.HTTPAddr,
			Handler: certmagic.DefaultACME.HTTPChallengeHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target := "https://" + s.cfg.Domain + r.URL.RequestURI()
				http.Redirect(w, r, target, http.StatusPermanentRedirect)
			})),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
	}

	errCh := make(chan error, 2)
	if httpSrv != nil && httpLn != nil {
		go func() {
			s.logger.Info("lune edge listening (ACME redirect/HTTP-01)", "addr", s.cfg.HTTPAddr)
			if err := httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	go func() {
		s.logger.Info("lune edge listening (ACME HTTPS)", "addr", s.cfg.ListenAddr, "domain", s.cfg.Domain, "staging", s.cfg.ACMEStaging)
		if err := httpsSrv.Serve(httpsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(shutdownCtx)
		}
		_ = httpsSrv.Shutdown(shutdownCtx)
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if httpSrv != nil {
			_ = httpSrv.Shutdown(shutdownCtx)
		}
		return httpsSrv.Shutdown(shutdownCtx)
	}
}
