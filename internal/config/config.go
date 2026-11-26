package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultTMDBAPIKey may be injected at build time via -ldflags
// (-X github.com/LuneServer/lune/internal/config.DefaultTMDBAPIKey=...) so release
// containers work out of the box without committing a key to source control.
// Setting LUNE_TMDB_API_KEY at runtime always takes precedence.
var DefaultTMDBAPIKey string

// EdgeMode controls how Lune binds and terminates HTTP/TLS traffic.
type EdgeMode string

const (
	EdgeModeLocal     EdgeMode = "local"
	EdgeModeDev       EdgeMode = "dev" // alias for local
	EdgeModeACME      EdgeMode = "acme"
	EdgeModeProxy     EdgeMode = "proxy"
	EdgeModeTailscale EdgeMode = "tailscale"
)

// Config holds the runtime configuration for Lune Server.
type Config struct {
	DataDir            string
	MediaDir           string
	WebDir             string
	EdgeMode           EdgeMode
	PublicURL          string
	ListenAddr         string
	HTTPAddr           string
	Domain             string
	ACMEEmail          string
	ACMEStaging        bool
	ACMEDNSProvider    string // "" (HTTP-01/ALPN-01) | "cloudflare" (DNS-01)
	CloudflareAPIToken string
	TMDBAPIKey         string
	TMDBBaseURL        string
	RequestsEnabled    *bool
	RadarrURL          string
	RadarrAPIKey       string
	SonarrURL          string
	SonarrAPIKey       string
	ScanInterval       time.Duration
	TrustedProxies     []netip.Prefix
	AdminLANOnly       bool
	IPAllowlist        []netip.Prefix
	IPBlocklist        []netip.Prefix
	LogLevel           string
	LogFormat          string
}

// Load reads configuration from environment variables with safe defaults.
func Load() (Config, error) {
	mode := EdgeMode(strings.ToLower(envOr("LUNE_EDGE_MODE", string(EdgeModeLocal))))
	switch mode {
	case EdgeModeLocal, EdgeModeDev, EdgeModeACME, EdgeModeProxy, EdgeModeTailscale:
	default:
		return Config{}, fmt.Errorf("invalid LUNE_EDGE_MODE %q: must be one of %q, %q, %q, %q (or %q)",
			mode, EdgeModeLocal, EdgeModeACME, EdgeModeProxy, EdgeModeTailscale, EdgeModeDev)
	}

	defaultListen := ":5863"
	if mode == EdgeModeACME {
		defaultListen = ":443"
	}

	staging, err := envBool("LUNE_ACME_STAGING", false)
	if err != nil {
		return Config{}, err
	}

	trusted, err := ParseCIDRs(os.Getenv("LUNE_TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid LUNE_TRUSTED_PROXIES: %w", err)
	}
	if len(trusted) == 0 {
		trusted = DefaultTrustedProxiesForMode(mode)
	}

	adminLANOnly, err := envBool("LUNE_ADMIN_LAN_ONLY", false)
	if err != nil {
		return Config{}, err
	}
	ipAllowlist, err := ParseCIDRs(os.Getenv("LUNE_IP_ALLOWLIST"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid LUNE_IP_ALLOWLIST: %w", err)
	}
	ipBlocklist, err := ParseCIDRs(os.Getenv("LUNE_IP_BLOCKLIST"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid LUNE_IP_BLOCKLIST: %w", err)
	}

	scanInterval := 30 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("LUNE_SCAN_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid LUNE_SCAN_INTERVAL %q: %w", raw, err)
		}
		scanInterval = d
	}

	var requestsEnabled *bool
	if raw := strings.TrimSpace(os.Getenv("LUNE_REQUESTS_ENABLED")); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid boolean LUNE_REQUESTS_ENABLED=%q: %w", raw, err)
		}
		requestsEnabled = &b
	}

	tmdbKey := strings.TrimSpace(os.Getenv("LUNE_TMDB_API_KEY"))
	tmdbBaseURL := strings.TrimSpace(os.Getenv("LUNE_TMDB_BASE_URL"))
	if tmdbKey == "" && tmdbBaseURL == "" {
		tmdbKey = strings.TrimSpace(DefaultTMDBAPIKey)
	}

	cfg := Config{
		DataDir:            envOr("LUNE_DATA_DIR", "./data"),
		MediaDir:           envOr("LUNE_MEDIA_DIR", "/media"),
		WebDir:             strings.TrimSpace(os.Getenv("LUNE_WEB_DIR")),
		EdgeMode:           mode,
		PublicURL:          strings.TrimRight(strings.TrimSpace(os.Getenv("LUNE_PUBLIC_URL")), "/"),
		ListenAddr:         envOr("LUNE_LISTEN_ADDR", defaultListen),
		HTTPAddr:           envOr("LUNE_HTTP_ADDR", ":80"),
		Domain:             strings.TrimSpace(os.Getenv("LUNE_DOMAIN")),
		ACMEEmail:          strings.TrimSpace(os.Getenv("LUNE_ACME_EMAIL")),
		ACMEStaging:        staging,
		ACMEDNSProvider:    strings.ToLower(strings.TrimSpace(os.Getenv("LUNE_ACME_DNS_PROVIDER"))),
		CloudflareAPIToken: strings.TrimSpace(os.Getenv("LUNE_CF_API_TOKEN")),
		TMDBAPIKey:         tmdbKey,
		TMDBBaseURL:        tmdbBaseURL,
		RequestsEnabled:    requestsEnabled,
		RadarrURL:          strings.TrimSpace(os.Getenv("LUNE_RADARR_URL")),
		RadarrAPIKey:       strings.TrimSpace(os.Getenv("LUNE_RADARR_API_KEY")),
		SonarrURL:          strings.TrimSpace(os.Getenv("LUNE_SONARR_URL")),
		SonarrAPIKey:       strings.TrimSpace(os.Getenv("LUNE_SONARR_API_KEY")),
		ScanInterval:       scanInterval,
		TrustedProxies:     trusted,
		AdminLANOnly:       adminLANOnly,
		IPAllowlist:        ipAllowlist,
		IPBlocklist:        ipBlocklist,
		LogLevel:           strings.ToLower(envOr("LUNE_LOG_LEVEL", "info")),
		LogFormat:          strings.ToLower(envOr("LUNE_LOG_FORMAT", "text")),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces invariants before the server starts.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("LUNE_DATA_DIR must not be empty")
	}
	if c.EdgeMode == EdgeModeACME && c.Domain == "" {
		return errors.New("LUNE_DOMAIN is required when LUNE_EDGE_MODE=acme")
	}
	if c.ACMEDNSProvider != "" && c.ACMEDNSProvider != "cloudflare" {
		return fmt.Errorf("unsupported LUNE_ACME_DNS_PROVIDER %q: supported values are empty or \"cloudflare\"", c.ACMEDNSProvider)
	}
	if c.ACMEDNSProvider == "cloudflare" && c.CloudflareAPIToken == "" {
		return errors.New("LUNE_CF_API_TOKEN is required when LUNE_ACME_DNS_PROVIDER=cloudflare")
	}
	return nil
}

// DBPath returns the SQLite database file path.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, "lune.db")
}

// CertDir returns the CertMagic storage path.
func (c Config) CertDir() string {
	return filepath.Join(c.DataDir, "certs")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean %s=%q: %w", key, v, err)
	}
	return b, nil
}
