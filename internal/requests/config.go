package requests

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/store"
)

const (
	requestsConfigKey = "requests_config"
	maskedAPIKeyValue = "••••••••"
)

// ArrInstanceConfig holds connection and provisioning settings for a single Radarr or Sonarr instance.
type ArrInstanceConfig struct {
	Enabled               bool   `json:"enabled"`
	URL                   string `json:"url"`
	APIKey                string `json:"api_key,omitempty"`
	HasAPIKey             bool   `json:"has_api_key"`
	QualityProfileID      int    `json:"quality_profile_id"`
	RootFolder            string `json:"root_folder"`
	AnimeQualityProfileID int    `json:"anime_quality_profile_id,omitempty"`
	AnimeRootFolder       string `json:"anime_root_folder,omitempty"`
	MinimumAvailability   string `json:"minimum_availability,omitempty"`
	SeriesType            string `json:"series_type,omitempty"`
	SeasonFolder          bool   `json:"season_folder"`
	SearchOnAdd           bool   `json:"search_on_add"`
}

// RequestsConfig holds the server-wide Media Requests configuration and optional *arr integration slots.
type RequestsConfig struct {
	Enabled        bool              `json:"enabled"`
	TMDBConfigured bool              `json:"tmdb_configured"`
	Radarr         ArrInstanceConfig `json:"radarr"`
	RadarrAnime    ArrInstanceConfig `json:"radarr_anime"`
	Sonarr         ArrInstanceConfig `json:"sonarr"`
	SonarrAnime    ArrInstanceConfig `json:"sonarr_anime"`
}

// Masked returns a copy of RequestsConfig with all raw API keys cleared and HasAPIKey populated.
func (c RequestsConfig) Masked() RequestsConfig {
	out := c
	out.Radarr = maskArrSlot(out.Radarr)
	out.RadarrAnime = maskArrSlot(out.RadarrAnime)
	out.Sonarr = maskArrSlot(out.Sonarr)
	out.SonarrAnime = maskArrSlot(out.SonarrAnime)
	return out
}

func maskArrSlot(slot ArrInstanceConfig) ArrInstanceConfig {
	slot.HasAPIKey = strings.TrimSpace(slot.APIKey) != ""
	slot.APIKey = ""
	return slot
}

func sameOrigin(a, b string) bool {
	ua, err := url.Parse(strings.TrimSpace(a))
	if err != nil || ua.Scheme == "" || ua.Host == "" {
		return false
	}
	ub, err := url.Parse(strings.TrimSpace(b))
	if err != nil || ub.Scheme == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func normalizeArrSlot(incoming, existing ArrInstanceConfig, isRadarr, isAnime bool) ArrInstanceConfig {
	out := incoming
	out.URL = strings.TrimRight(strings.TrimSpace(out.URL), "/")
	out.RootFolder = strings.TrimSpace(out.RootFolder)
	out.AnimeRootFolder = strings.TrimSpace(out.AnimeRootFolder)

	trimmedKey := strings.TrimSpace(out.APIKey)
	if trimmedKey == "" || trimmedKey == maskedAPIKeyValue {
		if sameOrigin(out.URL, existing.URL) {
			out.APIKey = existing.APIKey
		} else {
			out.APIKey = ""
		}
	} else {
		out.APIKey = trimmedKey
	}
	out.HasAPIKey = out.APIKey != ""

	if isRadarr {
		if strings.TrimSpace(out.MinimumAvailability) == "" {
			out.MinimumAvailability = "released"
		}
	} else {
		if strings.TrimSpace(out.SeriesType) == "" {
			if isAnime {
				out.SeriesType = "anime"
			} else {
				out.SeriesType = "standard"
			}
		}
	}
	return out
}

func defaultRequestsConfig() RequestsConfig {
	return RequestsConfig{
		Radarr: ArrInstanceConfig{
			MinimumAvailability: "released",
			SearchOnAdd:         true,
		},
		RadarrAnime: ArrInstanceConfig{
			MinimumAvailability: "released",
			SearchOnAdd:         true,
		},
		Sonarr: ArrInstanceConfig{
			SeriesType:   "standard",
			SeasonFolder: true,
			SearchOnAdd:  true,
		},
		SonarrAnime: ArrInstanceConfig{
			SeriesType:   "anime",
			SeasonFolder: true,
			SearchOnAdd:  true,
		},
	}
}

func loadStoredConfig(ctx context.Context, st *store.Store, envCfg config.Config, tmdbConfigured bool) (RequestsConfig, error) {
	cfg := defaultRequestsConfig()
	raw, err := st.GetSetting(ctx, requestsConfigKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return RequestsConfig{}, err
	}
	if err == nil && strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return RequestsConfig{}, err
		}
	}

	// Apply optional environment overrides when configured.
	if envCfg.RequestsEnabled != nil {
		cfg.Enabled = *envCfg.RequestsEnabled
	}
	if envCfg.RadarrURL != "" {
		cfg.Radarr.URL = strings.TrimRight(envCfg.RadarrURL, "/")
	}
	if envCfg.RadarrAPIKey != "" {
		cfg.Radarr.APIKey = envCfg.RadarrAPIKey
	}
	if envCfg.SonarrURL != "" {
		cfg.Sonarr.URL = strings.TrimRight(envCfg.SonarrURL, "/")
	}
	if envCfg.SonarrAPIKey != "" {
		cfg.Sonarr.APIKey = envCfg.SonarrAPIKey
	}

	cfg.TMDBConfigured = tmdbConfigured
	cfg.Radarr.HasAPIKey = strings.TrimSpace(cfg.Radarr.APIKey) != ""
	cfg.RadarrAnime.HasAPIKey = strings.TrimSpace(cfg.RadarrAnime.APIKey) != ""
	cfg.Sonarr.HasAPIKey = strings.TrimSpace(cfg.Sonarr.APIKey) != ""
	cfg.SonarrAnime.HasAPIKey = strings.TrimSpace(cfg.SonarrAnime.APIKey) != ""
	return cfg, nil
}
