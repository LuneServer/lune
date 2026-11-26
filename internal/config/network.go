package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// NetworkConfigSettingKey is the server_settings key storing runtime NetworkConfig JSON.
const NetworkConfigSettingKey = "network_config"

// NetworkConfig holds runtime-configurable Network / Access Mode settings
// persisted in server_settings["network_config"].
type NetworkConfig struct {
	EdgeMode        EdgeMode `json:"edge_mode"`
	ActiveEdgeMode  EdgeMode `json:"active_edge_mode,omitempty"`
	RestartRequired bool     `json:"restart_required"`
	PublicURL       string   `json:"public_url"`
	TrustedProxies  string   `json:"trusted_proxies"`
	Domain          string   `json:"domain"`
	ACMEEmail       string   `json:"acme_email"`
	ACMEStaging     bool     `json:"acme_staging"`
	ACMEDNSProvider string   `json:"acme_dns_provider"`
	CFAPIToken      string   `json:"cf_api_token,omitempty"`
	HasCFAPIToken   bool     `json:"has_cf_api_token"`
	AdminLANOnly    bool     `json:"admin_lan_only"`
	IPAllowlist     string   `json:"ip_allowlist"`
	IPBlocklist     string   `json:"ip_blocklist"`
}

// NormalizeEdgeMode trims, lowercases, and normalizes legacy/empty values ("", "dev") to "local".
func NormalizeEdgeMode(mode EdgeMode) EdgeMode {
	m := EdgeMode(strings.ToLower(strings.TrimSpace(string(mode))))
	if m == "" || m == EdgeModeDev {
		return EdgeModeLocal
	}
	return m
}

// DefaultTrustedProxiesForMode returns the default trusted proxy CIDRs for a given access mode.
func DefaultTrustedProxiesForMode(mode EdgeMode) []netip.Prefix {
	switch NormalizeEdgeMode(mode) {
	case EdgeModeProxy:
		p, _ := ParseCIDRs("127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16")
		return p
	case EdgeModeTailscale:
		p, _ := ParseCIDRs("127.0.0.0/8,::1/128")
		return p
	default:
		return nil
	}
}

// ParseCIDRs parses a comma-separated list of IP addresses or CIDR prefixes.
func ParseCIDRs(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]netip.Prefix, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			pfx, err := netip.ParsePrefix(p)
			if err != nil {
				return nil, fmt.Errorf("parse CIDR %q: %w", p, err)
			}
			out = append(out, pfx)
			continue
		}
		addr, err := netip.ParseAddr(p)
		if err != nil {
			return nil, fmt.Errorf("parse IP %q: %w", p, err)
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// FormatCIDRs formats a slice of prefixes as a comma-separated string.
func FormatCIDRs(prefixes []netip.Prefix) string {
	if len(prefixes) == 0 {
		return ""
	}
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// NetworkConfigFromConfig builds a NetworkConfig view from a startup Config.
func NetworkConfigFromConfig(cfg Config) NetworkConfig {
	mode := NormalizeEdgeMode(cfg.EdgeMode)
	trustedStr := ""
	if len(cfg.TrustedProxies) > 0 && !prefixesEqual(cfg.TrustedProxies, DefaultTrustedProxiesForMode(mode)) {
		trustedStr = FormatCIDRs(cfg.TrustedProxies)
	}
	return NetworkConfig{
		EdgeMode:        mode,
		PublicURL:       strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
		TrustedProxies:  trustedStr,
		Domain:          strings.TrimSpace(cfg.Domain),
		ACMEEmail:       strings.TrimSpace(cfg.ACMEEmail),
		ACMEStaging:     cfg.ACMEStaging,
		ACMEDNSProvider: strings.ToLower(strings.TrimSpace(cfg.ACMEDNSProvider)),
		CFAPIToken:      strings.TrimSpace(cfg.CloudflareAPIToken),
		HasCFAPIToken:   strings.TrimSpace(cfg.CloudflareAPIToken) != "",
		AdminLANOnly:    cfg.AdminLANOnly,
		IPAllowlist:     FormatCIDRs(cfg.IPAllowlist),
		IPBlocklist:     FormatCIDRs(cfg.IPBlocklist),
	}
}

// ApplyStoredNetworkConfig merges a persisted NetworkConfig onto a startup Config.
func ApplyStoredNetworkConfig(cfg Config, stored NetworkConfig) (Config, error) {
	mode := NormalizeEdgeMode(stored.EdgeMode)
	switch mode {
	case EdgeModeLocal, EdgeModeACME, EdgeModeProxy, EdgeModeTailscale:
	default:
		return Config{}, fmt.Errorf("invalid edge_mode %q: must be one of %q, %q, %q, %q",
			stored.EdgeMode, EdgeModeLocal, EdgeModeACME, EdgeModeProxy, EdgeModeTailscale)
	}

	publicURL := strings.TrimRight(strings.TrimSpace(stored.PublicURL), "/")
	if publicURL != "" {
		lower := strings.ToLower(publicURL)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			return Config{}, errors.New("public_url must start with http:// or https://")
		}
	}

	trusted, err := ParseCIDRs(stored.TrustedProxies)
	if err != nil {
		return Config{}, fmt.Errorf("invalid trusted_proxies: %w", err)
	}
	if len(trusted) == 0 {
		trusted = DefaultTrustedProxiesForMode(mode)
	}
	allowlist, err := ParseCIDRs(stored.IPAllowlist)
	if err != nil {
		return Config{}, fmt.Errorf("invalid ip_allowlist: %w", err)
	}
	blocklist, err := ParseCIDRs(stored.IPBlocklist)
	if err != nil {
		return Config{}, fmt.Errorf("invalid ip_blocklist: %w", err)
	}

	out := cfg
	out.EdgeMode = mode
	out.PublicURL = publicURL
	out.TrustedProxies = trusted
	out.AdminLANOnly = stored.AdminLANOnly
	out.IPAllowlist = allowlist
	out.IPBlocklist = blocklist
	out.Domain = strings.TrimSpace(stored.Domain)
	out.ACMEEmail = strings.TrimSpace(stored.ACMEEmail)
	out.ACMEStaging = stored.ACMEStaging
	out.ACMEDNSProvider = strings.ToLower(strings.TrimSpace(stored.ACMEDNSProvider))
	out.CloudflareAPIToken = strings.TrimSpace(stored.CFAPIToken)

	// Adjust ListenAddr automatically for ACME unless explicitly overridden via LUNE_LISTEN_ADDR.
	if strings.TrimSpace(os.Getenv("LUNE_LISTEN_ADDR")) == "" {
		if mode == EdgeModeACME {
			out.ListenAddr = ":443"
		} else if out.ListenAddr == ":443" || out.ListenAddr == "" {
			out.ListenAddr = ":5863"
		}
	}

	if err := out.Validate(); err != nil {
		return Config{}, err
	}
	return out, nil
}

func prefixesEqual(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
