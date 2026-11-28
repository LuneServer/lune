package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/segments"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/version"
)

const streamTokenTTL = 6 * time.Hour

var (
	ErrItemNotFound           = errors.New("media item not found")
	ErrFileMissing            = errors.New("media file missing on disk")
	ErrForbidden              = errors.New("forbidden")
	ErrUnauthorized           = errors.New("unauthorized")
	ErrInvalidStreamingConfig = errors.New("invalid streaming configuration")
)

// ClientCapabilities describes what the requesting player can decode natively.
type ClientCapabilities struct {
	Containers            []string
	VideoCodecs           []string
	AudioCodecs           []string
	Supports10BitH264     *bool
	Supports10BitHEVC     *bool
	SupportsHDR           *bool
	SupportsDoVi          *bool
	MaxAudioChannels      int
	MaxBitrate            int64
	MaxWidth              int
	MaxHeight             int
	AudioStreamIndex      *int
	BurnSubtitleStreamIdx *int
	StartPositionMS       *int64
	ForceTranscode        bool
	ClientName            string
}

// AudioTrackDTO describes an audio stream selectable during playback.
type AudioTrackDTO struct {
	Index         int    `json:"index"`
	Codec         string `json:"codec"`
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	ChannelLayout string `json:"channel_layout,omitempty"`
	Default       bool   `json:"default,omitempty"`
}

// SubtitleStreamDTO describes an available WebVTT subtitle URL (sidecar/embedded)
// or an embedded bitmap subtitle track (PGS/VobSub/DVB) that can be burned in during transcode.
type SubtitleStreamDTO struct {
	Index          int    `json:"index"`
	Language       string `json:"language"`
	Label          string `json:"label"`
	Format         string `json:"format,omitempty"` // "vtt" | "srt" | "ass" | "ssa" | "subrip" | "mov_text" | "hdmv_pgs_subtitle" | "dvd_subtitle"
	Source         string `json:"source,omitempty"` // "sidecar" | "embedded" | "burn_in"
	Forced         bool   `json:"forced,omitempty"`
	RequiresBurnIn bool   `json:"requires_burn_in,omitempty"`
	StreamIndex    *int   `json:"stream_index,omitempty"` // FFmpeg stream index for embedded/burn-in tracks
	URL            string `json:"url"`
}

// PlaybackPlan is returned to clients after capability negotiation.
type PlaybackPlan struct {
	SessionID             string                 `json:"session_id"`
	ItemID                string                 `json:"item_id"`
	Mode                  string                 `json:"mode"` // "direct" | "remux" | "transcode"
	Reason                string                 `json:"reason"`
	VideoCopy             bool                   `json:"video_copy"`
	AudioCopy             bool                   `json:"audio_copy"`
	HWAccel               string                 `json:"hw_accel,omitempty"`
	StreamURL             string                 `json:"stream_url"`
	MimeType              string                 `json:"mime_type"`
	AudioTracks           []AudioTrackDTO        `json:"audio_tracks"`
	Subtitles             []SubtitleStreamDTO    `json:"subtitles"`
	SelectedAudioIndex    *int                   `json:"selected_audio_index,omitempty"`
	SelectedSubtitleIndex *int                   `json:"selected_subtitle_index,omitempty"`
	BurnSubtitleStreamIdx *int                   `json:"burn_subtitle_stream_index,omitempty"`
	Chapters              []metadata.ChapterInfo `json:"chapters"`
	// Segments are the skippable spans (intro/recap/outro/preview): container chapters first,
	// IntroDB timestamps for kinds the chapters do not cover.
	Segments         []segments.Segment      `json:"segments"`
	Trickplay        *metadata.TrickplayInfo `json:"trickplay,omitempty"`
	TranscodeEnabled bool                    `json:"transcode_enabled"`
	ResumePositionMS int64                   `json:"resume_position_ms"`
	DurationMS       int64                   `json:"duration_ms"`
	ExpiresAt        time.Time               `json:"expires_at"`
}

// ActiveStream represents a currently active playback session visible to admins.
type ActiveStream struct {
	SessionID             string    `json:"session_id"`
	UserID                string    `json:"user_id"`
	Username              string    `json:"username"`
	ItemID                string    `json:"item_id"`
	ItemTitle             string    `json:"item_title"`
	Mode                  string    `json:"mode"`
	HWAccel               string    `json:"hw_accel,omitempty"`
	ClientName            string    `json:"client_name"`
	ClientIP              string    `json:"client_ip"`
	StartedAt             time.Time `json:"started_at"`
	LastSeenAt            time.Time `json:"last_seen_at"`
	MaxBitrate            int64     `json:"-"`
	MaxHeight             int       `json:"-"`
	VideoWidth            int       `json:"-"`
	SourceHeight          int       `json:"-"`
	Bitrate               int64     `json:"-"`
	DurationMS            int64     `json:"-"`
	VideoCodec            string    `json:"-"`
	VideoProfile          string    `json:"-"`
	VideoPixFmt           string    `json:"-"`
	VideoHDR              string    `json:"-"`
	DVProfile             int       `json:"-"`
	DVCompatID            int       `json:"-"`
	AudioCopy             bool      `json:"-"`
	AudioCodec            string    `json:"-"`
	AudioProfile          string    `json:"-"`
	AudioTitle            string    `json:"-"`
	AudioLanguage         string    `json:"-"`
	AudioChannels         int       `json:"-"`
	MaxAudioChannels      int       `json:"-"`
	AudioStreamIndex      *int      `json:"-"`
	BurnSubtitleStreamIdx *int      `json:"-"`
	StartPositionMS       int64     `json:"-"`
}

// Service manages playback negotiation, signed Direct Play, HLS transcoding, and watch progress.
type Service struct {
	store      *store.Store
	dataDir    string
	transcoder *Transcoder
	secret     []byte
	logger     *slog.Logger
	now        func() time.Time
	segments   *segments.Resolver

	mu      sync.Mutex
	active  map[string]*ActiveStream
	revoked map[string]time.Time
}

// NewService initializes the streaming & playback service and loads/generates the HMAC signing key.
func NewService(ctx context.Context, s *store.Store, dataDir string, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}

	hexSecret, err := s.GetOrCreateSetting(ctx, "stream_signing_secret", func() (string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	})
	if err != nil {
		return nil, fmt.Errorf("load stream signing secret: %w", err)
	}
	secret, err := hex.DecodeString(hexSecret)
	if err != nil || len(secret) < 16 {
		secret = []byte(hexSecret)
	}

	transcoder := NewTranscoder(dataDir, logger)
	resolver := segments.NewResolver(s, segments.NewIntroDBClient("Lune/"+version.Version), logger)
	if raw, err := s.GetSetting(ctx, StreamingConfigSettingKey); err == nil && strings.TrimSpace(raw) != "" {
		// Configs saved before introdb_enabled existed keep the default (on).
		cfg := StreamingConfig{IntroDBEnabled: true}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			logger.Warn("ignoring invalid persisted streaming_config", "error", err)
		} else {
			transcoder.UpdateConfig(cfg)
			resolver.SetEnabled(cfg.IntroDBEnabled)
		}
	}

	return &Service{
		store:      s,
		dataDir:    dataDir,
		transcoder: transcoder,
		secret:     secret,
		logger:     logger,
		now:        time.Now,
		segments:   resolver,
		active:     make(map[string]*ActiveStream),
		revoked:    make(map[string]time.Time),
	}, nil
}

// GetStreamingConfig returns the active live streaming/transcoding configuration.
func (s *Service) GetStreamingConfig() StreamingConfig {
	cfg := s.transcoder.GetConfig()
	cfg.IntroDBEnabled = s.segments.Enabled()
	return cfg
}

// SetSegmentResolver replaces the skip-segment resolver (tests inject a fake IntroDB).
func (s *Service) SetSegmentResolver(r *segments.Resolver) {
	s.segments = r
}

// ActiveTranscodeDirs returns the set of directory paths owned by live transcode sessions.
func (s *Service) ActiveTranscodeDirs() map[string]struct{} {
	if s == nil || s.transcoder == nil {
		return map[string]struct{}{}
	}
	return s.transcoder.ActiveDirs()
}

// PlaybackActivityWindow is how recently a playback session must have been seen (stream request,
// HLS fetch or progress heartbeat) to count as active playback for background-work throttling.
// Clients report progress about every 10s while playing; AVPlayer direct play can buffer for tens
// of seconds between range requests, so the window spans several missed heartbeats.
const PlaybackActivityWindow = 45 * time.Second

// HasActivePlayback reports whether any client is currently playing media: a live FFmpeg HLS
// session, or any negotiated session (direct, remux or transcode) seen within
// PlaybackActivityWindow. Direct play has no FFmpeg process, so it is only visible here.
func (s *Service) HasActivePlayback() bool {
	if s == nil {
		return false
	}
	if s.transcoder != nil && len(s.transcoder.ActiveDirs()) > 0 {
		return true
	}
	cutoff := s.now().UTC().Add(-PlaybackActivityWindow)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.active {
		if st.LastSeenAt.After(cutoff) {
			return true
		}
	}
	return false
}

// UpdateStreamingConfig validates, persists, and hot-reloads streaming/transcoding limits.
func (s *Service) UpdateStreamingConfig(ctx context.Context, actor store.User, cfg StreamingConfig, clientIP string) (StreamingConfig, error) {
	if cfg.IdleTimeoutMinutes == 0 {
		cfg.IdleTimeoutMinutes = int(defaultIdleTTL / time.Minute)
	}
	if cfg.MaxGlobalTranscodes < 0 || cfg.MaxGlobalTranscodes > 1000 {
		return StreamingConfig{}, fmt.Errorf("%w: max_global_transcodes must be between 0 and 1000", ErrInvalidStreamingConfig)
	}
	if cfg.MaxUserTranscodes < 0 || cfg.MaxUserTranscodes > 100 {
		return StreamingConfig{}, fmt.Errorf("%w: max_user_transcodes must be between 0 and 100", ErrInvalidStreamingConfig)
	}
	if cfg.IdleTimeoutMinutes < 1 || cfg.IdleTimeoutMinutes > 120 {
		return StreamingConfig{}, fmt.Errorf("%w: idle_timeout_minutes must be between 1 and 120", ErrInvalidStreamingConfig)
	}

	cfg.Transcoder = nil // diagnostics are runtime-only, never persisted
	raw, err := json.Marshal(cfg)
	if err != nil {
		return StreamingConfig{}, fmt.Errorf("marshal streaming config: %w", err)
	}
	if err := s.store.SetSetting(ctx, StreamingConfigSettingKey, string(raw)); err != nil {
		return StreamingConfig{}, fmt.Errorf("persist streaming config: %w", err)
	}

	s.transcoder.UpdateConfig(cfg)
	s.segments.SetEnabled(cfg.IntroDBEnabled)

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.streaming_config_updated",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail: fmt.Sprintf(
			"max_global_transcodes=%d max_user_transcodes=%d idle_timeout_minutes=%d introdb_enabled=%t",
			cfg.MaxGlobalTranscodes,
			cfg.MaxUserTranscodes,
			cfg.IdleTimeoutMinutes,
			cfg.IntroDBEnabled,
		),
	})

	return s.GetStreamingConfig(), nil
}

// Shutdown terminates active transcode workers cleanly.
func (s *Service) Shutdown() {
	if s.transcoder != nil {
		s.transcoder.Shutdown()
	}
}

// Negotiate selects direct/remux/transcode based on client capabilities and issues a signed stream URL.
func (s *Service) Negotiate(ctx context.Context, user store.User, itemID string, caps ClientCapabilities, clientIP string) (PlaybackPlan, error) {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return PlaybackPlan{}, ErrItemNotFound
		}
		return PlaybackPlan{}, err
	}

	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return PlaybackPlan{}, err
	}
	if !allowed {
		return PlaybackPlan{}, ErrForbidden
	}

	prefs := user.Preferences()
	if caps.MaxBitrate == 0 && prefs.MaxBitrate > 0 {
		caps.MaxBitrate = prefs.MaxBitrate
	}

	if _, err := os.Stat(item.FilePath); err != nil {
		return PlaybackPlan{}, ErrFileMissing
	}

	lib, _ := s.store.GetLibrary(ctx, item.LibraryID)
	isAnime := lib.Kind == "anime"

	var streams []metadata.StreamInfo
	if item.StreamsJSON != "" {
		_ = json.Unmarshal([]byte(item.StreamsJSON), &streams)
	}
	audioTracks := make([]AudioTrackDTO, 0)
	for _, st := range streams {
		if st.Type == "audio" {
			audioTracks = append(audioTracks, AudioTrackDTO{
				Index:         st.Index,
				Codec:         st.Codec,
				Language:      st.Language,
				Title:         st.Title,
				Channels:      st.Channels,
				ChannelLayout: st.ChannelLayout,
				Default:       st.Default,
			})
		}
	}

	var chapters []metadata.ChapterInfo
	if item.ChaptersJSON != "" {
		_ = json.Unmarshal([]byte(item.ChaptersJSON), &chapters)
	}
	if chapters == nil {
		chapters = []metadata.ChapterInfo{}
	}
	metadata.PopulateChapterThumbnails(s.dataDir, item.ID, chapters)

	// Resolve skip segments (may wait briefly on an uncached IntroDB lookup) alongside the rest
	// of negotiation, and warm the next episode's lookup so it is cached when Up Next plays.
	segmentsCh := make(chan []segments.Segment, 1)
	go func() { segmentsCh <- s.segments.Resolve(ctx, item, chapters) }()
	if item.Kind == "episode" && s.segments.Enabled() {
		go func() {
			nctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if next, err := s.store.GetNextEpisode(nctx, item.ID); err == nil {
				s.segments.Prefetch(next)
			}
		}()
	}

	// Build full subtitle list (sidecar + embedded text tracks when FFmpeg is available).
	// Token is needed for URLs, so first compute preferred audio track to decide playback mode.
	var sidecarTracks []library.SubtitleTrack
	if item.SubtitlesJSON != "" {
		_ = json.Unmarshal([]byte(item.SubtitlesJSON), &sidecarTracks)
	}

	selectedAudioIdx, selectedAudioLang := selectPreferredAudio(audioTracks, caps.AudioStreamIndex, prefs, isAnime)

	var selectedAudioTrack *AudioTrackDTO
	if selectedAudioIdx != nil {
		for i := range audioTracks {
			if audioTracks[i].Index == *selectedAudioIdx {
				selectedAudioTrack = &audioTracks[i]
				break
			}
		}
	}

	// Validate optional bitmap subtitle burn-in index against probed streams so clients cannot pass arbitrary stream indices.
	var validBurnSubIdx *int
	if caps.BurnSubtitleStreamIdx != nil && *caps.BurnSubtitleStreamIdx >= 0 && s.transcoder.Available() {
		for _, st := range streams {
			if st.Index == *caps.BurnSubtitleStreamIdx && st.Type == "subtitle" && isBitmapSubtitleCodec(st.Codec) {
				idx := st.Index
				validBurnSubIdx = &idx
				break
			}
		}
	}

	// Only pass AudioStreamIndex to evaluatePlayback when the selected stream is not already
	// the primary (first) audio stream, avoiding unnecessary remuxing for Direct Play files.
	effectiveCaps := caps
	effectiveCaps.BurnSubtitleStreamIdx = validBurnSubIdx
	if selectedAudioIdx != nil {
		if len(audioTracks) > 0 && audioTracks[0].Index == *selectedAudioIdx {
			effectiveCaps.AudioStreamIndex = nil
		} else {
			effectiveCaps.AudioStreamIndex = selectedAudioIdx
		}
	}

	decision := evaluatePlayback(item, streams, selectedAudioTrack, effectiveCaps, s.transcoder.Available())
	mode, reason := decision.mode, decision.reason
	if mode != "direct" {
		if err := s.transcoder.CheckLimits(user.ID); err != nil {
			return PlaybackPlan{}, err
		}
	}

	var resumeMS int64
	if caps.StartPositionMS != nil && *caps.StartPositionMS >= 0 {
		resumeMS = *caps.StartPositionMS
	} else if prog, err := s.store.GetPlaybackProgress(ctx, user.ID, item.ID); err == nil && !prog.Completed {
		resumeMS = prog.PositionMS
	}

	// Keep seekPositionMS as the exact -ss target passed to FFmpeg, while snapping resumeMS
	// (returned to the client as its timeline/subtitle offset) to the actual start offset that
	// FFmpeg's -noaccurate_seek -ss <seekPositionMS> + -avoid_negative_ts make_zero shifts to 0.000s.
	seekPositionMS := resumeMS
	if mode == "remux" && seekPositionMS > 0 {
		if startOffsetMS, ok := s.transcoder.ResolveSeekKeyframeMS(ctx, item.FilePath, seekPositionMS); ok {
			resumeMS = startOffsetMS
		}
	}

	now := s.now().UTC()
	exp := now.Add(streamTokenTTL)
	sessionID := uuid.NewString()

	token, err := SignToken(s.secret, TokenClaims{
		SessionID: sessionID,
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      mode,
		ExpiresAt: exp.Unix(),
	})
	if err != nil {
		return PlaybackPlan{}, err
	}

	selectedAudioCodec := item.AudioCodec
	selectedAudioChannels := item.AudioChannels
	selectedAudioProfile := ""
	selectedAudioTitle := ""
	selectedAudioLanguage := selectedAudioLang
	if selectedAudioTrack != nil {
		if selectedAudioTrack.Codec != "" {
			selectedAudioCodec = selectedAudioTrack.Codec
		}
		if selectedAudioTrack.Channels > 0 {
			selectedAudioChannels = selectedAudioTrack.Channels
		}
		selectedAudioTitle = selectedAudioTrack.Title
		if selectedAudioLanguage == "" {
			selectedAudioLanguage = metadata.NormalizeLanguage(selectedAudioTrack.Language)
		}
		for _, st := range streams {
			if st.Type == "audio" && st.Index == selectedAudioTrack.Index {
				selectedAudioProfile = st.Profile
				break
			}
		}
	} else {
		for _, st := range streams {
			if st.Type == "audio" {
				selectedAudioProfile = st.Profile
				selectedAudioTitle = st.Title
				if selectedAudioLanguage == "" {
					selectedAudioLanguage = metadata.NormalizeLanguage(st.Language)
				}
				break
			}
		}
	}

	streamURL := "/stream/direct/" + token
	mimeType := mimeForContainer(item.Container, item.FilePath)
	hwAccel := ""
	if mode != "direct" {
		if decision.audioCopy && isDolbyAtmosAudio(selectedAudioCodec, selectedAudioProfile, selectedAudioTitle) {
			streamURL = "/stream/hls/" + token + "/master.m3u8"
		} else {
			streamURL = "/stream/hls/" + token + "/index.m3u8"
		}
		mimeType = "application/vnd.apple.mpegurl"
		if mode == "transcode" {
			hwAccel = string(s.transcoder.HWAccel())
		}
	}

	clientName := caps.ClientName
	if clientName == "" {
		clientName = "Lune Client"
	}

	var replaced []string
	s.mu.Lock()
	for id, existing := range s.active {
		if existing.UserID == user.ID &&
			existing.ItemID == item.ID &&
			existing.ClientName == clientName &&
			existing.ClientIP == clientIP {
			delete(s.active, id)
			replaced = append(replaced, id)
		}
	}
	var videoProfile, videoPixFmt string
	var dvProfile, dvCompatID int
	for _, st := range streams {
		if st.Type == "video" {
			videoProfile = st.Profile
			videoPixFmt = st.PixFmt
			dvProfile = st.DVProfile
			dvCompatID = st.DVCompatID
			break
		}
	}

	s.active[sessionID] = &ActiveStream{
		SessionID:             sessionID,
		UserID:                user.ID,
		Username:              user.Username,
		ItemID:                item.ID,
		ItemTitle:             item.Title,
		Mode:                  mode,
		HWAccel:               hwAccel,
		ClientName:            clientName,
		ClientIP:              clientIP,
		StartedAt:             now,
		LastSeenAt:            now,
		MaxBitrate:            effectiveCaps.MaxBitrate,
		MaxHeight:             effectiveCaps.MaxHeight,
		VideoWidth:            item.VideoWidth,
		SourceHeight:          item.VideoHeight,
		Bitrate:               item.Bitrate,
		DurationMS:            item.DurationMS,
		VideoCodec:            item.VideoCodec,
		VideoProfile:          videoProfile,
		VideoPixFmt:           videoPixFmt,
		VideoHDR:              item.VideoHDR,
		DVProfile:             dvProfile,
		DVCompatID:            dvCompatID,
		AudioCopy:             decision.audioCopy,
		AudioCodec:            selectedAudioCodec,
		AudioProfile:          selectedAudioProfile,
		AudioTitle:            selectedAudioTitle,
		AudioLanguage:         selectedAudioLanguage,
		AudioChannels:         selectedAudioChannels,
		MaxAudioChannels:      effectiveCaps.MaxAudioChannels,
		AudioStreamIndex:      effectiveCaps.AudioStreamIndex,
		BurnSubtitleStreamIdx: validBurnSubIdx,
		StartPositionMS:       seekPositionMS,
	}
	s.mu.Unlock()
	for _, oldID := range replaced {
		s.transcoder.StopAsync(oldID)
	}
	s.logger.Info("playback session started",
		"session_id", sessionID,
		"user", user.Username,
		"item", item.Title,
		"mode", mode,
		"replaced", len(replaced),
	)

	subDTOs := buildSubtitleCatalog(sidecarTracks, streams, s.transcoder.Available(), token)
	var selectedSubIdx *int
	if validBurnSubIdx != nil {
		for _, sub := range subDTOs {
			if sub.RequiresBurnIn && sub.StreamIndex != nil && *sub.StreamIndex == *validBurnSubIdx {
				idx := sub.Index
				selectedSubIdx = &idx
				break
			}
		}
	} else {
		selectedSubIdx = selectPreferredSubtitle(subDTOs, selectedAudioLang, prefs, isAnime)
	}

	return PlaybackPlan{
		SessionID:             sessionID,
		ItemID:                item.ID,
		Mode:                  mode,
		Reason:                reason,
		VideoCopy:             decision.videoCopy,
		AudioCopy:             decision.audioCopy,
		HWAccel:               hwAccel,
		StreamURL:             streamURL,
		MimeType:              mimeType,
		AudioTracks:           audioTracks,
		Subtitles:             subDTOs,
		SelectedAudioIndex:    selectedAudioIdx,
		SelectedSubtitleIndex: selectedSubIdx,
		BurnSubtitleStreamIdx: validBurnSubIdx,
		Chapters:              chapters,
		Segments:              <-segmentsCh,
		Trickplay:             metadata.LoadTrickplay(s.dataDir, item.ID),
		TranscodeEnabled:      s.transcoder.Available(),
		ResumePositionMS:      resumeMS,
		DurationMS:            item.DurationMS,
		ExpiresAt:             exp,
	}, nil
}

// DirectLink holds short-lived HMAC-signed URLs for external player streaming or original file download.
type DirectLink struct {
	StreamURL   string    `json:"stream_url"`
	DownloadURL string    `json:"download_url"`
	Filename    string    `json:"filename"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ItemTrackSelection holds pre-play audio/subtitle catalogs and user-preference default selections.
type ItemTrackSelection struct {
	AudioTracks           []AudioTrackDTO
	Subtitles             []SubtitleStreamDTO
	SelectedAudioIndex    *int
	SelectedSubtitleIndex *int
}

// InspectItemTracks builds the audio/subtitle track list and smart defaults for an item detail page
// using the exact same preference & anime Sub/Dub rules as Negotiate.
func (s *Service) InspectItemTracks(ctx context.Context, user store.User, item store.MediaItem) ItemTrackSelection {
	var streams []metadata.StreamInfo
	if item.StreamsJSON != "" {
		_ = json.Unmarshal([]byte(item.StreamsJSON), &streams)
	}
	audioTracks := make([]AudioTrackDTO, 0)
	for _, st := range streams {
		if st.Type == "audio" {
			audioTracks = append(audioTracks, AudioTrackDTO{
				Index:         st.Index,
				Codec:         st.Codec,
				Language:      metadata.NormalizeLanguage(st.Language),
				Title:         st.Title,
				Channels:      st.Channels,
				ChannelLayout: st.ChannelLayout,
				Default:       st.Default,
			})
		}
	}

	var sidecarTracks []library.SubtitleTrack
	if item.SubtitlesJSON != "" {
		_ = json.Unmarshal([]byte(item.SubtitlesJSON), &sidecarTracks)
	}
	if len(sidecarTracks) == 0 && item.FilePath != "" {
		sidecarTracks = library.DiscoverSubtitles(item.FilePath)
	}

	isAnime := false
	if lib, err := s.store.GetLibrary(ctx, item.LibraryID); err == nil && lib.Kind == "anime" {
		isAnime = true
	}

	ffmpegAvail := s.transcoder != nil && s.transcoder.Available()
	subDTOs := buildSubtitleCatalog(sidecarTracks, streams, ffmpegAvail, "")
	prefs := user.Preferences()
	selectedAudioIdx, selectedAudioLang := selectPreferredAudio(audioTracks, nil, prefs, isAnime)
	selectedSubIdx := selectPreferredSubtitle(subDTOs, selectedAudioLang, prefs, isAnime)

	return ItemTrackSelection{
		AudioTracks:           audioTracks,
		Subtitles:             subDTOs,
		SelectedAudioIndex:    selectedAudioIdx,
		SelectedSubtitleIndex: selectedSubIdx,
	}
}

// CreateDirectLink generates short-lived HMAC-signed direct stream and attachment download URLs.
func (s *Service) CreateDirectLink(ctx context.Context, user store.User, itemID string) (DirectLink, error) {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return DirectLink{}, ErrItemNotFound
		}
		return DirectLink{}, err
	}
	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return DirectLink{}, err
	}
	if !allowed {
		return DirectLink{}, ErrForbidden
	}
	if _, err := os.Stat(item.FilePath); err != nil {
		return DirectLink{}, ErrFileMissing
	}

	now := s.now().UTC()
	exp := now.Add(streamTokenTTL)
	token, err := SignToken(s.secret, TokenClaims{
		SessionID: uuid.NewString(),
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      "direct",
		ExpiresAt: exp.Unix(),
	})
	if err != nil {
		return DirectLink{}, err
	}
	streamURL := "/stream/direct/" + token
	return DirectLink{
		StreamURL:   streamURL,
		DownloadURL: streamURL + "?download=1",
		Filename:    filepath.Base(item.FilePath),
		ExpiresAt:   exp,
	}, nil
}

func (s *Service) verifyActiveUser(ctx context.Context, userID string) error {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil || u.Disabled {
		return ErrForbidden
	}
	return nil
}

// ServeDirect streams a verified media file directly off disk with full HTTP Range support.
func (s *Service) ServeDirect(w http.ResponseWriter, r *http.Request, token string) {
	claims, err := VerifyToken(s.secret, token, s.now())
	if err != nil {
		http.Error(w, "invalid or expired stream token", http.StatusUnauthorized)
		return
	}
	if claims.Mode != "direct" {
		http.Error(w, "token mode mismatch", http.StatusBadRequest)
		return
	}
	if s.IsSessionRevoked(claims.SessionID) {
		http.Error(w, "stream session was stopped", http.StatusUnauthorized)
		return
	}
	if err := s.verifyActiveUser(r.Context(), claims.UserID); err != nil {
		http.Error(w, "account disabled or unavailable", http.StatusForbidden)
		return
	}

	item, err := s.store.GetMediaItem(r.Context(), claims.ItemID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(item.FilePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	s.touchOrRestoreDirect(r.Context(), claims, item, r.RemoteAddr)
	baseName := filepath.Base(item.FilePath)
	w.Header().Set("Content-Type", mimeForContainer(item.Container, item.FilePath))
	w.Header().Set("Cache-Control", "private, no-store")
	if r.URL.Query().Get("download") == "1" {
		safeName := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(baseName)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
	}
	http.ServeContent(w, r, baseName, info.ModTime(), f)
}

// ServeHLS serves an on-demand FFmpeg HLS playlist or segment for a verified stream token.
func (s *Service) ServeHLS(w http.ResponseWriter, r *http.Request, token, fileName string) {
	claims, err := VerifyToken(s.secret, token, s.now())
	if err != nil {
		http.Error(w, "invalid or expired stream token", http.StatusUnauthorized)
		return
	}
	if claims.Mode != "remux" && claims.Mode != "transcode" {
		http.Error(w, "token mode mismatch", http.StatusBadRequest)
		return
	}
	if err := s.verifyActiveUser(r.Context(), claims.UserID); err != nil {
		http.Error(w, "account disabled or unavailable", http.StatusForbidden)
		return
	}

	opts := TranscodeOptions{Mode: claims.Mode}
	s.mu.Lock()
	act, ok := s.active[claims.SessionID]
	if ok {
		opts.MaxBitrate = act.MaxBitrate
		opts.MaxHeight = act.MaxHeight
		opts.VideoWidth = act.VideoWidth
		opts.SourceHeight = act.SourceHeight
		opts.Bitrate = act.Bitrate
		opts.DurationMS = act.DurationMS
		opts.VideoCodec = act.VideoCodec
		opts.VideoProfile = act.VideoProfile
		opts.VideoPixFmt = act.VideoPixFmt
		opts.VideoHDR = act.VideoHDR
		opts.DVProfile = act.DVProfile
		opts.DVCompatID = act.DVCompatID
		opts.AudioCopy = act.AudioCopy
		opts.AudioCodec = act.AudioCodec
		opts.AudioProfile = act.AudioProfile
		opts.AudioTitle = act.AudioTitle
		opts.AudioLanguage = act.AudioLanguage
		opts.AudioChannels = act.AudioChannels
		opts.MaxAudioChannels = act.MaxAudioChannels
		opts.AudioStreamIndex = act.AudioStreamIndex
		opts.BurnSubtitleStreamIdx = act.BurnSubtitleStreamIdx
		opts.StartPositionMS = act.StartPositionMS
		act.LastSeenAt = s.now().UTC()
	}
	s.mu.Unlock()
	if !ok {
		http.Error(w, "stream session is no longer active", http.StatusUnauthorized)
		return
	}

	if fileName == "master.m3u8" {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(buildMasterPlaylist(opts)))
		return
	}

	item, err := s.store.GetMediaItem(r.Context(), claims.ItemID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	filePath, err := s.transcoder.EnsureSession(r.Context(), claims.SessionID, claims.UserID, item.FilePath, opts, fileName)
	if err != nil {
		if errors.Is(err, ErrInvalidSegment) {
			http.Error(w, "invalid segment", http.StatusBadRequest)
			return
		}
		if errors.Is(err, ErrTooManyTranscodes) {
			http.Error(w, "too many concurrent transcode sessions", http.StatusTooManyRequests)
			return
		}
		if errors.Is(err, ErrFFmpegUnavailable) {
			http.Error(w, "ffmpeg unavailable for transcoding", http.StatusNotImplemented)
			return
		}
		http.Error(w, "stream unavailable", http.StatusBadGateway)
		return
	}
	// Reflect actual runtime HWAccel on ActiveStream in case EnsureSession fell back to software.
	if claims.Mode == "transcode" {
		if actualHW := s.transcoder.SessionHWAccel(claims.SessionID); actualHW != "" {
			s.mu.Lock()
			if a, exists := s.active[claims.SessionID]; exists {
				a.HWAccel = string(actualHW)
			}
			s.mu.Unlock()
		}
	}

	switch {
	case strings.HasSuffix(fileName, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
	case strings.HasSuffix(fileName, ".m4s"), strings.HasSuffix(fileName, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "private, max-age=3600")
	default:
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}
	http.ServeFile(w, r, filePath)
}

// ServeSubtitle serves a verified sidecar (.vtt/.srt/.ass/.ssa) or embedded text subtitle stream as WebVTT.
func (s *Service) ServeSubtitle(w http.ResponseWriter, r *http.Request, token, indexStr string) {
	claims, err := VerifyToken(s.secret, token, s.now())
	if err != nil {
		http.Error(w, "invalid or expired stream token", http.StatusUnauthorized)
		return
	}
	if err := s.verifyActiveUser(r.Context(), claims.UserID); err != nil {
		http.Error(w, "account disabled or unavailable", http.StatusForbidden)
		return
	}
	item, err := s.store.GetMediaItem(r.Context(), claims.ItemID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	trimmed := strings.TrimSuffix(indexStr, ".vtt")
	if strings.HasPrefix(trimmed, "embedded-") {
		streamIdx, err := strconv.Atoi(strings.TrimPrefix(trimmed, "embedded-"))
		if err != nil || streamIdx < 0 {
			http.NotFound(w, r)
			return
		}
		var streams []metadata.StreamInfo
		if err := json.Unmarshal([]byte(item.StreamsJSON), &streams); err != nil {
			http.NotFound(w, r)
			return
		}
		valid := false
		codec := ""
		for _, st := range streams {
			if st.Index == streamIdx && st.Type == "subtitle" && isTextSubtitleCodec(st.Codec) {
				valid = true
				codec = st.Codec
				break
			}
		}
		if !valid {
			http.NotFound(w, r)
			return
		}
		cacheKey := fmt.Sprintf("%s_%d", item.ID, item.FileMtime)
		var startSec float64
		if rawStart := strings.TrimSpace(r.URL.Query().Get("start")); rawStart != "" {
			if parsed, pErr := strconv.ParseFloat(rawStart, 64); pErr == nil && parsed > 0 {
				startSec = parsed
			}
		}
		if err := s.transcoder.StreamSubtitleVTT(r.Context(), w, r, cacheKey, item.FilePath, streamIdx, codec, startSec); err != nil {
			if errors.Is(err, ErrFFmpegUnavailable) {
				http.Error(w, "ffmpeg unavailable for embedded subtitle extraction", http.StatusNotImplemented)
				return
			}
			http.Error(w, "failed to extract subtitle", http.StatusBadGateway)
			return
		}
		return
	}

	idx, err := strconv.Atoi(trimmed)
	if err != nil || idx < 0 {
		http.NotFound(w, r)
		return
	}

	var tracks []library.SubtitleTrack
	if err := json.Unmarshal([]byte(item.SubtitlesJSON), &tracks); err != nil {
		http.NotFound(w, r)
		return
	}

	var found *library.SubtitleTrack
	for i := range tracks {
		if tracks[i].Index == idx {
			found = &tracks[i]
			break
		}
	}
	if found == nil {
		http.NotFound(w, r)
		return
	}

	raw, err := os.ReadFile(found.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	out := raw
	switch strings.ToLower(found.Format) {
	case "srt":
		out = SRTToWebVTT(raw)
	case "ass", "ssa":
		out = ASSToWebVTT(raw)
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(out)
}

// IsSessionRevoked reports whether sessionID was explicitly stopped.
func (s *Service) IsSessionRevoked(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	_, ok := s.revoked[sessionID]
	s.mu.Unlock()
	return ok
}

func (s *Service) markRevokedLocked(sessionID string) {
	if sessionID == "" {
		return
	}
	now := s.now().UTC()
	s.revoked[sessionID] = now
	// Prune expired revocations older than streamTokenTTL to keep memory bounded.
	cutoff := now.Add(-streamTokenTTL)
	for id, ts := range s.revoked {
		if ts.Before(cutoff) {
			delete(s.revoked, id)
		}
	}
}

// StopSession terminates an active playback/transcode session.
func (s *Service) StopSession(sessionID string) {
	s.mu.Lock()
	delete(s.active, sessionID)
	s.markRevokedLocked(sessionID)
	s.mu.Unlock()
	s.transcoder.Stop(sessionID)
}

// StopSessionForUser terminates an active playback session if owned by user or if user is admin.
func (s *Service) StopSessionForUser(sessionID string, user store.User) error {
	s.mu.Lock()
	if act, ok := s.active[sessionID]; ok {
		if act.UserID != user.ID && user.Role != "admin" {
			s.mu.Unlock()
			return ErrForbidden
		}
		delete(s.active, sessionID)
		s.markRevokedLocked(sessionID)
	} else if user.Role != "admin" {
		s.mu.Unlock()
		return nil
	} else {
		s.markRevokedLocked(sessionID)
	}
	s.mu.Unlock()
	s.transcoder.Stop(sessionID)
	s.logger.Info("playback session stopped", "session_id", sessionID, "user", user.Username)
	return nil
}

// ListActiveStreams returns active streams seen within the last 15 minutes.
func (s *Service) ListActiveStreams() []ActiveStream {
	cutoff := s.now().UTC().Add(-15 * time.Minute)
	s.mu.Lock()
	var expired []string
	out := make([]ActiveStream, 0, len(s.active))
	for id, st := range s.active {
		if st.LastSeenAt.Before(cutoff) {
			delete(s.active, id)
			expired = append(expired, id)
			continue
		}
		out = append(out, *st)
	}
	s.mu.Unlock()

	for _, id := range expired {
		s.transcoder.Stop(id)
	}
	return out
}

func (s *Service) UpdateProgress(ctx context.Context, user store.User, itemID string, positionMS int64, completed *bool) (store.PlaybackProgress, error) {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.PlaybackProgress{}, ErrItemNotFound
		}
		return store.PlaybackProgress{}, err
	}
	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return store.PlaybackProgress{}, err
	}
	if !allowed {
		return store.PlaybackProgress{}, ErrForbidden
	}
	s.touchActiveByUserItem(user.ID, itemID)
	return s.store.UpsertPlaybackProgress(ctx, user.ID, itemID, positionMS, item.DurationMS, completed)
}

func (s *Service) GetProgress(ctx context.Context, user store.User, itemID string) (store.PlaybackProgress, error) {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.PlaybackProgress{}, ErrItemNotFound
		}
		return store.PlaybackProgress{}, err
	}
	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return store.PlaybackProgress{}, err
	}
	if !allowed {
		return store.PlaybackProgress{}, ErrForbidden
	}
	p, err := s.store.GetPlaybackProgress(ctx, user.ID, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return store.PlaybackProgress{UserID: user.ID, ItemID: itemID}, nil
	}
	return p, err
}

func (s *Service) ClearProgress(ctx context.Context, user store.User, itemID string) error {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrItemNotFound
		}
		return err
	}
	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return s.store.DeletePlaybackProgress(ctx, user.ID, itemID)
}

func (s *Service) ContinueWatching(ctx context.Context, user store.User, limit int) ([]store.ContinueWatchingEntry, error) {
	allowedIDs, unrestricted, err := s.store.AccessibleLibraryIDs(ctx, user)
	if err != nil {
		return nil, err
	}
	return s.store.ListContinueWatching(ctx, user.ID, limit, allowedIDs, !unrestricted)
}

func (s *Service) NextUp(ctx context.Context, user store.User, limit int) ([]store.NextUpEntry, error) {
	allowedIDs, unrestricted, err := s.store.AccessibleLibraryIDs(ctx, user)
	if err != nil {
		return nil, err
	}
	if unrestricted {
		allowedIDs = nil
	} else if allowedIDs == nil {
		allowedIDs = []string{}
	}
	return s.store.ListNextUp(ctx, user.ID, allowedIDs, limit)
}

func (s *Service) touchActive(sessionID string) {
	s.mu.Lock()
	if act, ok := s.active[sessionID]; ok {
		act.LastSeenAt = s.now().UTC()
	}
	s.mu.Unlock()
}

func (s *Service) touchActiveByUserItem(userID, itemID string) {
	now := s.now().UTC()
	s.mu.Lock()
	for _, act := range s.active {
		if act.UserID == userID && act.ItemID == itemID {
			act.LastSeenAt = now
		}
	}
	s.mu.Unlock()
}

func (s *Service) touchOrRestoreDirect(ctx context.Context, claims TokenClaims, item store.MediaItem, remoteAddr string) {
	now := s.now().UTC()
	s.mu.Lock()
	if act, ok := s.active[claims.SessionID]; ok {
		act.LastSeenAt = now
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	username := ""
	if u, err := s.store.GetUserByID(ctx, claims.UserID); err == nil {
		username = u.Username
	}
	s.mu.Lock()
	if act, ok := s.active[claims.SessionID]; ok {
		act.LastSeenAt = now
	} else {
		s.active[claims.SessionID] = &ActiveStream{
			SessionID:  claims.SessionID,
			UserID:     claims.UserID,
			Username:   username,
			ItemID:     item.ID,
			ItemTitle:  item.Title,
			Mode:       "direct",
			ClientName: "Lune Client",
			ClientIP:   remoteAddr,
			StartedAt:  now,
			LastSeenAt: now,
		}
	}
	s.mu.Unlock()
}

type playbackDecision struct {
	mode      string
	reason    string
	videoCopy bool
	audioCopy bool
}

func choosePlaybackMode(item store.MediaItem, caps ClientCapabilities, ffmpegAvailable bool) (string, string) {
	d := evaluatePlayback(item, nil, nil, caps, ffmpegAvailable)
	return d.mode, d.reason
}

func evaluatePlayback(item store.MediaItem, streams []metadata.StreamInfo, selectedAudio *AudioTrackDTO, caps ClientCapabilities, ffmpegAvailable bool) playbackDecision {
	var videoProfile, videoPixFmt, videoCodecTag string
	var dvProfile, dvCompatID int
	for _, st := range streams {
		if st.Type == "video" {
			videoProfile = st.Profile
			videoPixFmt = st.PixFmt
			videoCodecTag = st.CodecTag
			dvProfile = st.DVProfile
			dvCompatID = st.DVCompatID
			break
		}
	}

	audioCodec := item.AudioCodec
	audioChannels := item.AudioChannels
	if selectedAudio != nil {
		if selectedAudio.Codec != "" {
			audioCodec = selectedAudio.Codec
		}
		if selectedAudio.Channels > 0 {
			audioChannels = selectedAudio.Channels
		}
	}

	audioCodecOK := len(caps.AudioCodecs) == 0 || audioCodec == "" || containsFold(caps.AudioCodecs, audioCodec)
	audioChOK := caps.MaxAudioChannels == 0 || audioChannels == 0 || audioChannels <= caps.MaxAudioChannels
	audioOK := audioCodecOK && audioChOK
	audioCopyHLS := audioOK && canCopyAudioInFMP4(audioCodec)

	// Bitmap subtitle burn-in (PGS / VobSub / DVB) always requires video transcoding.
	if caps.BurnSubtitleStreamIdx != nil && ffmpegAvailable {
		return playbackDecision{
			mode:      "transcode",
			reason:    "video transcode for bitmap subtitle burn-in",
			videoCopy: false,
			audioCopy: audioCopyHLS,
		}
	}

	if caps.ForceTranscode && ffmpegAvailable {
		return playbackDecision{
			mode:      "transcode",
			reason:    "user selected quality transcode",
			videoCopy: false,
			audioCopy: audioCopyHLS,
		}
	}

	// If client sent no capability constraints and no non-default audio track, default to Direct Play.
	if caps.AudioStreamIndex == nil &&
		len(caps.Containers) == 0 && len(caps.VideoCodecs) == 0 && len(caps.AudioCodecs) == 0 &&
		caps.Supports10BitH264 == nil && caps.Supports10BitHEVC == nil &&
		caps.SupportsHDR == nil && caps.SupportsDoVi == nil &&
		caps.MaxAudioChannels == 0 && caps.MaxBitrate == 0 && caps.MaxWidth == 0 && caps.MaxHeight == 0 {
		return playbackDecision{mode: "direct", reason: "direct play (default client profile)", videoCopy: true, audioCopy: true}
	}

	containerOK := len(caps.Containers) == 0 || item.Container == "" || containsFold(caps.Containers, item.Container)
	// Safari/Apple/MSE players reject MP4/M4V/MOV files tagged as "hev1" (require "hvc1"); remux fixes the tag cheaply.
	if containerOK &&
		(strings.EqualFold(item.VideoCodec, "hevc") || strings.EqualFold(item.VideoCodec, "h265")) &&
		strings.EqualFold(videoCodecTag, "hev1") &&
		isMP4FamilyContainer(item.Container) {
		containerOK = false
	}

	videoCodecOK := len(caps.VideoCodecs) == 0 || item.VideoCodec == "" || containsFold(caps.VideoCodecs, item.VideoCodec)

	hasHDR10BaseLayer := strings.EqualFold(item.VideoHDR, "dovi_hdr10") ||
		dvProfile == 7 || (dvProfile == 8 && (dvCompatID == 1 || dvCompatID == 6))
	isDoVi := strings.EqualFold(item.VideoHDR, "dovi") || strings.EqualFold(item.VideoHDR, "dovi_hdr10")

	profileOK := true
	profileReason := ""
	if (strings.EqualFold(item.VideoCodec, "h264") || strings.EqualFold(item.VideoCodec, "avc")) &&
		is10BitVideo(videoProfile, videoPixFmt) && caps.Supports10BitH264 != nil && !*caps.Supports10BitH264 {
		profileOK = false
		profileReason = "10-bit H.264 requires transcoding for this client"
	} else if (strings.EqualFold(item.VideoCodec, "hevc") || strings.EqualFold(item.VideoCodec, "h265")) &&
		(is10BitVideo(videoProfile, videoPixFmt) || isHDRSource(item.VideoHDR)) && caps.Supports10BitHEVC != nil && !*caps.Supports10BitHEVC {
		profileOK = false
		profileReason = "10-bit HEVC requires transcoding for this client"
	} else if isDoVi && caps.SupportsDoVi != nil && !*caps.SupportsDoVi {
		if hasHDR10BaseLayer && caps.SupportsHDR != nil && *caps.SupportsHDR {
			// Dolby Vision Profile 7/8 with HDR10 base layer can remux cleanly to HDR10 hvc1 without video transcoding.
			containerOK = false
		} else {
			profileOK = false
			profileReason = "Dolby Vision requires SDR transcoding for this client"
		}
	} else if isHDRSource(item.VideoHDR) && !isDoVi && caps.SupportsHDR != nil && !*caps.SupportsHDR {
		profileOK = false
		profileReason = "HDR video requires SDR transcoding for this client"
	}

	videoOK := videoCodecOK && profileOK
	resOK := (caps.MaxWidth == 0 || item.VideoWidth <= caps.MaxWidth) && (caps.MaxHeight == 0 || item.VideoHeight <= caps.MaxHeight)
	bitrateOK := caps.MaxBitrate == 0 || item.Bitrate == 0 || item.Bitrate <= caps.MaxBitrate

	if caps.AudioStreamIndex == nil && containerOK && videoOK && audioOK && resOK && bitrateOK {
		return playbackDecision{mode: "direct", reason: "direct play", videoCopy: true, audioCopy: true}
	}
	if !ffmpegAvailable {
		return playbackDecision{mode: "direct", reason: "direct play fallback (ffmpeg unavailable)", videoCopy: true, audioCopy: true}
	}
	if videoOK && resOK && bitrateOK && canRemuxVideoInFMP4(item.VideoCodec) {
		reason := "direct stream video copy with container/audio remux"
		if caps.AudioStreamIndex != nil && audioCopyHLS {
			reason = "direct stream remux for selected audio track"
		} else if audioCopyHLS {
			reason = "direct stream zero-transcode container remux"
		}
		return playbackDecision{
			mode:      "remux",
			reason:    reason,
			videoCopy: true,
			audioCopy: audioCopyHLS,
		}
	}
	reason := "video transcode required for client codec/resolution/bitrate"
	if profileReason != "" {
		reason = profileReason
	} else if videoOK && resOK && bitrateOK && !canRemuxVideoInFMP4(item.VideoCodec) {
		reason = "video codec requires HLS transcoding"
	}
	return playbackDecision{
		mode:      "transcode",
		reason:    reason,
		videoCopy: false,
		audioCopy: audioCopyHLS,
	}
}

func is10BitProfile(profile string) bool {
	p := strings.ToLower(strings.TrimSpace(profile))
	return strings.Contains(p, "10") || strings.Contains(p, "rext") || p == "profile 2"
}

func is10BitPixFmt(pixFmt string) bool {
	pf := strings.ToLower(strings.TrimSpace(pixFmt))
	return strings.Contains(pf, "10le") || strings.Contains(pf, "10be") || strings.Contains(pf, "p010") ||
		strings.Contains(pf, "12le") || strings.Contains(pf, "12be")
}

func is10BitVideo(profile, pixFmt string) bool {
	return is10BitProfile(profile) || is10BitPixFmt(pixFmt)
}

func isMP4FamilyContainer(container string) bool {
	switch strings.ToLower(strings.TrimSpace(container)) {
	case "mp4", "m4v", "mov":
		return true
	default:
		return false
	}
}

func canRemuxVideoInFMP4(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "", "h264", "avc", "hevc", "h265", "av1":
		return true
	default:
		return false
	}
}

func canCopyAudioInFMP4(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "ac3", "eac3", "mp3":
		return true
	default:
		return false
	}
}

func containsFold(list []string, target string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), target) {
			return true
		}
	}
	return false
}

func mimeForContainer(container, path string) string {
	ext := strings.ToLower(container)
	if ext == "" {
		ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
	switch ext {
	case "mp4", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mkv", "matroska":
		return "video/x-matroska"
	case "mov":
		return "video/quicktime"
	case "ts", "m2ts":
		return "video/mp2t"
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "m4a":
		return "audio/mp4"
	case "opus", "ogg":
		return "audio/ogg"
	default:
		return "application/octet-stream"
	}
}

func isTextSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "ass", "ssa", "subrip", "srt", "webvtt", "vtt", "mov_text", "text":
		return true
	default:
		return false
	}
}

func isBitmapSubtitleCodec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "hdmv_pgs_subtitle", "pgssub", "dvd_subtitle", "vobsub", "dvb_subtitle", "dvbsub":
		return true
	default:
		return false
	}
}

func isSignsOrForcedTitle(title string) bool {
	lower := strings.ToLower(title)
	return strings.Contains(lower, "sign") || strings.Contains(lower, "song") || strings.Contains(lower, "forced")
}

func formatEmbeddedSubtitleLabel(st metadata.StreamInfo, lang string, forced bool) string {
	names := map[string]string{
		"en": "English", "ja": "Japanese", "fr": "French", "es": "Spanish",
		"de": "German", "it": "Italian", "ko": "Korean", "pt": "Portuguese",
		"zh": "Chinese", "ru": "Russian", "ar": "Arabic",
	}
	base := names[lang]
	if base == "" {
		if lang != "" {
			base = strings.ToUpper(lang)
		} else {
			base = "Subtitles"
		}
	}
	title := strings.TrimSpace(st.Title)
	if title != "" && !strings.EqualFold(title, base) && !strings.EqualFold(title, lang) {
		return base + " · " + title
	}
	if forced {
		return base + " (Forced)"
	}
	return base
}

func buildSubtitleCatalog(sidecars []library.SubtitleTrack, streams []metadata.StreamInfo, ffmpegAvailable bool, token string) []SubtitleStreamDTO {
	out := make([]SubtitleStreamDTO, 0, len(sidecars)+4)
	nextIdx := 0
	for _, tr := range sidecars {
		idx := tr.Index
		if idx >= nextIdx {
			nextIdx = idx + 1
		}
		lang := metadata.NormalizeLanguage(tr.Language)
		if lang == "" {
			lang = "und"
		}
		url := ""
		if token != "" {
			url = fmt.Sprintf("/stream/sub/%s/%d.vtt", token, tr.Index)
		}
		out = append(out, SubtitleStreamDTO{
			Index:    tr.Index,
			Language: lang,
			Label:    tr.Label,
			Format:   tr.Format,
			Source:   "sidecar",
			Forced:   tr.Forced,
			URL:      url,
		})
	}

	if ffmpegAvailable {
		for _, st := range streams {
			if st.Type != "subtitle" {
				continue
			}
			isText := isTextSubtitleCodec(st.Codec)
			isBitmap := isBitmapSubtitleCodec(st.Codec)
			if !isText && !isBitmap {
				continue
			}
			lang := metadata.NormalizeLanguage(st.Language)
			if lang == "" {
				lang = "und"
			}
			forced := st.Forced || isSignsOrForcedTitle(st.Title)
			stIdx := st.Index
			if isText {
				url := ""
				if token != "" {
					url = fmt.Sprintf("/stream/sub/%s/embedded-%d.vtt", token, st.Index)
				}
				out = append(out, SubtitleStreamDTO{
					Index:       nextIdx,
					Language:    lang,
					Label:       formatEmbeddedSubtitleLabel(st, lang, forced),
					Format:      strings.ToLower(st.Codec),
					Source:      "embedded",
					Forced:      forced,
					StreamIndex: &stIdx,
					URL:         url,
				})
			} else {
				out = append(out, SubtitleStreamDTO{
					Index:          nextIdx,
					Language:       lang,
					Label:          formatEmbeddedSubtitleLabel(st, lang, forced),
					Format:         strings.ToLower(st.Codec),
					Source:         "burn_in",
					Forced:         forced,
					RequiresBurnIn: true,
					StreamIndex:    &stIdx,
					URL:            "",
				})
			}
			nextIdx++
		}
	}
	return out
}

func selectPreferredAudio(tracks []AudioTrackDTO, explicitIdx *int, prefs store.UserPreferences, isAnime bool) (*int, string) {
	if len(tracks) == 0 {
		return explicitIdx, ""
	}
	if explicitIdx != nil {
		for _, tr := range tracks {
			if tr.Index == *explicitIdx {
				return explicitIdx, metadata.NormalizeLanguage(tr.Language)
			}
		}
		return explicitIdx, ""
	}

	desiredLang := metadata.NormalizeLanguage(prefs.AudioLanguage)
	switch animeAudioMode(prefs, isAnime) {
	case store.AnimeAudioSubbed:
		desiredLang = "ja"
	case store.AnimeAudioDubbed:
		desiredLang = mainLanguage(prefs)
	}

	if desiredLang != "" {
		for _, tr := range tracks {
			if metadata.NormalizeLanguage(tr.Language) == desiredLang {
				idx := tr.Index
				return &idx, desiredLang
			}
		}
	}

	for _, tr := range tracks {
		if tr.Default {
			idx := tr.Index
			return &idx, metadata.NormalizeLanguage(tr.Language)
		}
	}
	idx := tracks[0].Index
	return &idx, metadata.NormalizeLanguage(tracks[0].Language)
}

// animeAudioMode returns the user's anime_audio mode ("subbed" unless "dubbed") for anime
// items, "" otherwise.
func animeAudioMode(prefs store.UserPreferences, isAnime bool) string {
	if !isAnime {
		return ""
	}
	if mode, ok := store.NormalizeAnimeAudio(prefs.AnimeAudio); ok {
		return mode
	}
	return store.AnimeAudioSubbed
}

// mainLanguage is the language a dubbed viewer wants to hear: their audio language unless it
// is Japanese (a Japanese "dub" is just the original), else their subtitle language, else
// English.
func mainLanguage(prefs store.UserPreferences) string {
	if l := metadata.NormalizeLanguage(prefs.AudioLanguage); l != "" && l != "ja" {
		return l
	}
	if l := metadata.NormalizeLanguage(prefs.SubtitleLanguage); l != "" {
		return l
	}
	return "en"
}

func selectPreferredSubtitle(subs []SubtitleStreamDTO, audioLang string, prefs store.UserPreferences, isAnime bool) *int {
	if len(subs) == 0 {
		return nil
	}
	prefSubLang := metadata.NormalizeLanguage(prefs.SubtitleLanguage)
	if prefSubLang == "" {
		prefSubLang = "en"
	}

	// Determine whether full dialogue subtitles should auto-enable:
	// 1. User explicitly enabled subtitles in preferences, OR
	// 2. The audio is in a known language other than the subtitle language (a Korean film, anime
	//    in Japanese), so the viewer needs full subtitles to follow it.
	//
	// For anime, anime_audio decides and subtitles_enabled is ignored: subbed shows full
	// subtitles unless the audio is already in the subtitle language, dubbed shows them only when
	// no dub was found and the audio fell back to another language.
	var wantFull bool
	switch animeAudioMode(prefs, isAnime) {
	case store.AnimeAudioSubbed:
		wantFull = audioLang != prefSubLang
	case store.AnimeAudioDubbed:
		wantFull = audioLang != "" && audioLang != prefSubLang
	default:
		wantFull = prefs.SubtitlesEnabled || (audioLang != "" && audioLang != prefSubLang)
	}
	// Subtitles that only transcribe the audio's own language don't help a viewer who doesn't
	// speak it; skip them in the fallback unless the user asked for subtitles on everything.
	alwaysOn := prefs.SubtitlesEnabled && animeAudioMode(prefs, isAnime) == ""

	if wantFull {
		// Prefer non-forced (full dialogue) text track matching preferred subtitle language.
		// Never auto-select RequiresBurnIn tracks so we don't force an unexpected transcode.
		for _, s := range subs {
			if !s.RequiresBurnIn && !s.Forced && s.Language == prefSubLang {
				idx := s.Index
				return &idx
			}
		}
		// Fallback to any other non-forced text track (e.g. "und" sidecar).
		for _, s := range subs {
			if !alwaysOn && audioLang != "" && s.Language == audioLang {
				continue
			}
			if !s.RequiresBurnIn && !s.Forced {
				idx := s.Index
				return &idx
			}
		}
	}

	// Otherwise (e.g. English Dubbed anime or standard movies), prefer a Forced / Signs & Songs text track
	// matching preferred language so on-screen text is still translated!
	for _, s := range subs {
		if !s.RequiresBurnIn && s.Forced && s.Language == prefSubLang {
			idx := s.Index
			return &idx
		}
	}
	for _, s := range subs {
		if !s.RequiresBurnIn && s.Forced {
			idx := s.Index
			return &idx
		}
	}
	return nil
}
