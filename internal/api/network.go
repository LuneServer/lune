package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
)

const maskedCFTokenValue = "••••••••"

type acmeSnapshot struct {
	domain          string
	acmeEmail       string
	acmeStaging     bool
	acmeDNSProvider string
	cfAPIToken      string
}

// runtimeNetwork holds thread-safe live network state and coordinates hot-reloading
// of trusted proxies, effective access mode, and public URL.
type runtimeNetwork struct {
	mu            sync.RWMutex
	activeMode    config.EdgeMode
	effectiveMode config.EdgeMode
	publicURL     string
	adminLANOnly  bool
	acmeSettings  acmeSnapshot
	ipResolver    *security.ClientIPResolver
	edgeGuard     *security.EdgeGuard
}

func effectivePublicURLForMode(mode config.EdgeMode, publicURL, domain string) string {
	switch config.NormalizeEdgeMode(mode) {
	case config.EdgeModeLocal:
		return ""
	case config.EdgeModeACME:
		d := strings.TrimSpace(domain)
		if d == "" {
			return ""
		}
		return "https://" + d
	case config.EdgeModeProxy, config.EdgeModeTailscale:
		return strings.TrimRight(strings.TrimSpace(publicURL), "/")
	default:
		return ""
	}
}

func newRuntimeNetwork(cfg config.Config, resolver *security.ClientIPResolver, guard *security.EdgeGuard) *runtimeNetwork {
	mode := config.NormalizeEdgeMode(cfg.EdgeMode)
	return &runtimeNetwork{
		activeMode:    mode,
		effectiveMode: mode,
		publicURL:     effectivePublicURLForMode(mode, cfg.PublicURL, cfg.Domain),
		adminLANOnly:  cfg.AdminLANOnly,
		acmeSettings: acmeSnapshot{
			domain:          strings.TrimSpace(cfg.Domain),
			acmeEmail:       strings.TrimSpace(cfg.ACMEEmail),
			acmeStaging:     cfg.ACMEStaging,
			acmeDNSProvider: strings.ToLower(strings.TrimSpace(cfg.ACMEDNSProvider)),
			cfAPIToken:      strings.TrimSpace(cfg.CloudflareAPIToken),
		},
		ipResolver: resolver,
		edgeGuard:  guard,
	}
}

func (r *runtimeNetwork) AdminLANOnly() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.adminLANOnly
}

func (r *runtimeNetwork) ActiveMode() config.EdgeMode {
	if r == nil {
		return config.EdgeModeLocal
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeMode
}

func (r *runtimeNetwork) EffectiveMode() config.EdgeMode {
	if r == nil {
		return config.EdgeModeLocal
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectiveMode
}

func (r *runtimeNetwork) PublicURL() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.publicURL
}

func (r *runtimeNetwork) RestartRequiredFor(saved config.NetworkConfig) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	activeMode := r.activeMode
	bootACME := r.acmeSettings
	r.mu.RUnlock()

	activeIsACME := activeMode == config.EdgeModeACME
	savedIsACME := config.NormalizeEdgeMode(saved.EdgeMode) == config.EdgeModeACME
	if activeIsACME != savedIsACME {
		return true
	}
	if activeIsACME {
		return strings.TrimSpace(saved.Domain) != bootACME.domain ||
			strings.TrimSpace(saved.ACMEEmail) != bootACME.acmeEmail ||
			saved.ACMEStaging != bootACME.acmeStaging ||
			strings.ToLower(strings.TrimSpace(saved.ACMEDNSProvider)) != bootACME.acmeDNSProvider ||
			strings.TrimSpace(saved.CFAPIToken) != bootACME.cfAPIToken
	}
	return false
}

func (r *runtimeNetwork) ApplyLive(saved config.NetworkConfig, trusted, allowlist, blocklist []netip.Prefix, restartRequired bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.adminLANOnly = saved.AdminLANOnly
	if !restartRequired {
		r.effectiveMode = config.NormalizeEdgeMode(saved.EdgeMode)
	}
	r.publicURL = effectivePublicURLForMode(r.effectiveMode, saved.PublicURL, saved.Domain)
	resolver := r.ipResolver
	guard := r.edgeGuard
	r.mu.Unlock()

	if guard != nil {
		guard.UpdateLists(allowlist, blocklist)
	}
	if !restartRequired && resolver != nil {
		resolver.UpdateTrusted(trusted)
	}
}

type NetworkConfigResponse struct {
	Body config.NetworkConfig
}

type UpdateNetworkConfigInput struct {
	Body struct {
		EdgeMode        string `json:"edge_mode"`
		PublicURL       string `json:"public_url,omitempty"`
		TrustedProxies  string `json:"trusted_proxies,omitempty"`
		Domain          string `json:"domain,omitempty"`
		ACMEEmail       string `json:"acme_email,omitempty"`
		ACMEStaging     bool   `json:"acme_staging,omitempty"`
		ACMEDNSProvider string `json:"acme_dns_provider,omitempty"`
		CFAPIToken      string `json:"cf_api_token,omitempty"`
		AdminLANOnly    bool   `json:"admin_lan_only,omitempty"`
		IPAllowlist     string `json:"ip_allowlist,omitempty"`
		IPBlocklist     string `json:"ip_blocklist,omitempty"`
	}
}

type IPBansListResponse struct {
	Body struct {
		Bans []security.IPBan `json:"bans"`
	}
}

type BanIPInput struct {
	Body struct {
		IP              string `json:"ip" minLength:"3" maxLength:"64"`
		Reason          string `json:"reason,omitempty" maxLength:"200"`
		DurationMinutes int    `json:"duration_minutes,omitempty" minimum:"0" maximum:"525600"`
	}
}

type UnbanIPInput struct {
	IP string `path:"ip"`
}

type BanActionResponse struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func loadCurrentNetworkConfig(ctx context.Context, st *store.Store, baseCfg config.Config) (config.NetworkConfig, error) {
	if st != nil {
		raw, err := st.GetSetting(ctx, config.NetworkConfigSettingKey)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return config.NetworkConfig{}, err
		}
		if err == nil && strings.TrimSpace(raw) != "" {
			var stored config.NetworkConfig
			if err := json.Unmarshal([]byte(raw), &stored); err != nil {
				return config.NetworkConfig{}, err
			}
			stored.EdgeMode = config.NormalizeEdgeMode(stored.EdgeMode)
			stored.HasCFAPIToken = strings.TrimSpace(stored.CFAPIToken) != ""
			return stored, nil
		}
	}
	return config.NetworkConfigFromConfig(baseCfg), nil
}

func maskNetworkConfig(cfg config.NetworkConfig, runtimeNet *runtimeNetwork, restartRequired bool) config.NetworkConfig {
	out := cfg
	out.EdgeMode = config.NormalizeEdgeMode(out.EdgeMode)
	out.HasCFAPIToken = strings.TrimSpace(cfg.CFAPIToken) != ""
	out.CFAPIToken = ""
	out.ActiveEdgeMode = runtimeNet.ActiveMode()
	out.RestartRequired = restartRequired
	return out
}

func registerNetworkAdminRoutes(api huma.API, baseCfg config.Config, st *store.Store, runtimeNet *runtimeNetwork) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-get-network-config",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/network/config",
		Summary:     "Get server network and access mode configuration (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*NetworkConfigResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}

		cfg, err := loadCurrentNetworkConfig(ctx, st, baseCfg)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load network configuration", err)
		}
		restartRequired := runtimeNet.RestartRequiredFor(cfg)
		return &NetworkConfigResponse{
			Body: maskNetworkConfig(cfg, runtimeNet, restartRequired),
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-update-network-config",
		Method:      http.MethodPut,
		Path:        "/api/v1/admin/network/config",
		Summary:     "Update server network and access mode configuration (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UpdateNetworkConfigInput) (*NetworkConfigResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}

		existing, err := loadCurrentNetworkConfig(ctx, st, baseCfg)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load existing network configuration", err)
		}

		mode := config.NormalizeEdgeMode(config.EdgeMode(in.Body.EdgeMode))
		switch mode {
		case config.EdgeModeLocal, config.EdgeModeACME, config.EdgeModeProxy, config.EdgeModeTailscale:
		default:
			return nil, huma.Error422UnprocessableEntity(
				fmt.Sprintf("invalid edge_mode %q: must be one of local, acme, proxy, tailscale", in.Body.EdgeMode),
			)
		}

		publicURL := strings.TrimRight(strings.TrimSpace(in.Body.PublicURL), "/")
		if publicURL != "" {
			lower := strings.ToLower(publicURL)
			if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
				return nil, huma.Error422UnprocessableEntity("public_url must start with http:// or https://")
			}
		}

		trustedRaw := strings.TrimSpace(in.Body.TrustedProxies)
		parsedTrusted, err := config.ParseCIDRs(trustedRaw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("invalid trusted_proxies: %v", err))
		}
		effectiveTrusted := parsedTrusted
		if len(effectiveTrusted) == 0 {
			effectiveTrusted = config.DefaultTrustedProxiesForMode(mode)
		}

		allowlistRaw := strings.TrimSpace(in.Body.IPAllowlist)
		parsedAllowlist, err := config.ParseCIDRs(allowlistRaw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("invalid ip_allowlist: %v", err))
		}
		blocklistRaw := strings.TrimSpace(in.Body.IPBlocklist)
		parsedBlocklist, err := config.ParseCIDRs(blocklistRaw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("invalid ip_blocklist: %v", err))
		}

		// Prevent an admin from locking themselves out when enabling LAN-only admin mode from a public IP.
		if in.Body.AdminLANOnly {
			if callerIP, err := netip.ParseAddr(strings.TrimSpace(clientIPFromContext(ctx))); err == nil {
				callerIP = callerIP.Unmap()
				inAllow := false
				for _, p := range parsedAllowlist {
					if p.Contains(callerIP) {
						inAllow = true
						break
					}
				}
				if !security.IsPrivateOrLocalIP(callerIP) && !inAllow {
					return nil, huma.Error422UnprocessableEntity(
						fmt.Sprintf("cannot enable LAN-only Admin Console from public IP %s unless it is added to IP Allowlist first", callerIP.String()),
					)
				}
			}
		}

		domain := strings.TrimSpace(in.Body.Domain)
		if mode == config.EdgeModeACME && domain == "" {
			return nil, huma.Error422UnprocessableEntity("domain is required when edge_mode is acme")
		}

		acmeEmail := strings.TrimSpace(in.Body.ACMEEmail)
		dnsProvider := strings.ToLower(strings.TrimSpace(in.Body.ACMEDNSProvider))
		if dnsProvider != "" && dnsProvider != "cloudflare" {
			return nil, huma.Error422UnprocessableEntity(
				fmt.Sprintf("unsupported acme_dns_provider %q: supported values are empty or \"cloudflare\"", dnsProvider),
			)
		}

		cfToken := strings.TrimSpace(in.Body.CFAPIToken)
		if cfToken == "" || cfToken == maskedCFTokenValue {
			cfToken = strings.TrimSpace(existing.CFAPIToken)
		}
		if dnsProvider == "cloudflare" && cfToken == "" {
			return nil, huma.Error422UnprocessableEntity("cf_api_token is required when acme_dns_provider is cloudflare")
		}

		toStore := config.NetworkConfig{
			EdgeMode:        mode,
			PublicURL:       publicURL,
			TrustedProxies:  trustedRaw,
			Domain:          domain,
			ACMEEmail:       acmeEmail,
			ACMEStaging:     in.Body.ACMEStaging,
			ACMEDNSProvider: dnsProvider,
			CFAPIToken:      cfToken,
			HasCFAPIToken:   cfToken != "",
			AdminLANOnly:    in.Body.AdminLANOnly,
			IPAllowlist:     allowlistRaw,
			IPBlocklist:     blocklistRaw,
		}

		raw, err := json.Marshal(toStore)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to encode network configuration", err)
		}
		if err := st.SetSetting(ctx, config.NetworkConfigSettingKey, string(raw)); err != nil {
			return nil, huma.Error500InternalServerError("failed to save network configuration", err)
		}

		restartRequired := runtimeNet.RestartRequiredFor(toStore)
		runtimeNet.ApplyLive(toStore, effectiveTrusted, parsedAllowlist, parsedBlocklist, restartRequired)

		_ = st.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "admin.network_config_updated",
			ActorID:   actor.ID,
			ClientIP:  clientIPFromContext(ctx),
			Detail:    fmt.Sprintf("edge_mode=%s admin_lan_only=%t restart_required=%t", toStore.EdgeMode, toStore.AdminLANOnly, restartRequired),
		})

		return &NetworkConfigResponse{
			Body: maskNetworkConfig(toStore, runtimeNet, restartRequired),
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-ip-bans",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/security/bans",
		Summary:     "List currently active automatic and manual IP bans (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*IPBansListResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		out := &IPBansListResponse{}
		if runtimeNet != nil && runtimeNet.edgeGuard != nil {
			out.Body.Bans = runtimeNet.edgeGuard.ListBans()
		} else {
			out.Body.Bans = []security.IPBan{}
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-ban-ip",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/security/bans",
		Summary:     "Manually ban an IP address (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *BanIPInput) (*BanActionResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if runtimeNet == nil || runtimeNet.edgeGuard == nil {
			return nil, huma.Error500InternalServerError("edge security guard unavailable")
		}
		dur := time.Duration(in.Body.DurationMinutes) * time.Minute
		if err := runtimeNet.edgeGuard.BanIP(in.Body.IP, in.Body.Reason, dur); err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		out := &BanActionResponse{}
		out.Body.OK = true
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-unban-ip",
		Method:      http.MethodDelete,
		Path:        "/api/v1/admin/security/bans/{ip}",
		Summary:     "Remove an active IP ban (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UnbanIPInput) (*BanActionResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if runtimeNet != nil && runtimeNet.edgeGuard != nil {
			runtimeNet.edgeGuard.UnbanIP(in.IP)
		}
		_ = st.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "admin.ip_unbanned",
			ActorID:   actor.ID,
			ClientIP:  clientIPFromContext(ctx),
			Detail:    fmt.Sprintf("unbanned IP %s", in.IP),
		})
		out := &BanActionResponse{}
		out.Body.OK = true
		return out, nil
	})
}
