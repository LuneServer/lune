package config

import (
	"testing"
)

func TestApplyStoredNetworkConfig(t *testing.T) {
	t.Setenv("LUNE_LISTEN_ADDR", "")

	base := Config{
		DataDir:    "/data",
		EdgeMode:   EdgeModeLocal,
		ListenAddr: ":5863",
	}

	// Switching to ACME updates ListenAddr to :443 when LUNE_LISTEN_ADDR is unset.
	acmeCfg, err := ApplyStoredNetworkConfig(base, NetworkConfig{
		EdgeMode:        EdgeModeACME,
		Domain:          "lune.example.com",
		ACMEEmail:       "admin@example.com",
		ACMEDNSProvider: "cloudflare",
		CFAPIToken:      "cf-token",
	})
	if err != nil {
		t.Fatalf("ApplyStoredNetworkConfig(acme): %v", err)
	}
	if acmeCfg.EdgeMode != EdgeModeACME || acmeCfg.ListenAddr != ":443" || acmeCfg.Domain != "lune.example.com" {
		t.Fatalf("unexpected acme config: %+v", acmeCfg)
	}

	// Switching to proxy populates default trusted proxies when blank and resets :443 -> :5863.
	proxyCfg, err := ApplyStoredNetworkConfig(acmeCfg, NetworkConfig{
		EdgeMode:  EdgeModeProxy,
		PublicURL: "https://media.example.com/",
	})
	if err != nil {
		t.Fatalf("ApplyStoredNetworkConfig(proxy): %v", err)
	}
	if proxyCfg.EdgeMode != EdgeModeProxy || proxyCfg.ListenAddr != ":5863" || proxyCfg.PublicURL != "https://media.example.com" {
		t.Fatalf("unexpected proxy config: %+v", proxyCfg)
	}
	if len(proxyCfg.TrustedProxies) == 0 {
		t.Fatal("expected default trusted proxies for proxy mode")
	}

	// Respects explicit LUNE_LISTEN_ADDR override.
	t.Setenv("LUNE_LISTEN_ADDR", ":9443")
	overrideBase := Config{
		DataDir:    "/data",
		EdgeMode:   EdgeModeLocal,
		ListenAddr: ":9443",
	}
	overrideACME, err := ApplyStoredNetworkConfig(overrideBase, NetworkConfig{
		EdgeMode: EdgeModeACME,
		Domain:   "lune.example.com",
	})
	if err != nil {
		t.Fatalf("ApplyStoredNetworkConfig(override): %v", err)
	}
	if overrideACME.ListenAddr != ":9443" {
		t.Fatalf("expected explicit LUNE_LISTEN_ADDR :9443 to be preserved, got %q", overrideACME.ListenAddr)
	}
}

func TestDefaultTrustedProxiesForTailscale(t *testing.T) {
	got := FormatCIDRs(DefaultTrustedProxiesForMode(EdgeModeTailscale))
	if got != "127.0.0.0/8,::1/128" {
		t.Fatalf("Tailscale default trusted proxies = %q, want %q", got, "127.0.0.0/8,::1/128")
	}
}
