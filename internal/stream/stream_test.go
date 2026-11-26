package stream

import (
	"strings"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

func TestSignAndVerifyToken(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC)

	tok, err := SignToken(secret, TokenClaims{
		SessionID: "sess-1",
		UserID:    "user-1",
		ItemID:    "item-1",
		Mode:      "direct",
		ExpiresAt: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}

	claims, err := VerifyToken(secret, tok, now)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if claims.ItemID != "item-1" || claims.Mode != "direct" {
		t.Fatalf("unexpected claims: %+v", claims)
	}

	// Tampered token rejected.
	if _, err := VerifyToken(secret, tok+"x", now); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for tampered token, got %v", err)
	}

	// Expired token rejected.
	if _, err := VerifyToken(secret, tok, now.Add(2*time.Hour)); err != ErrExpiredToken {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestChoosePlaybackMode(t *testing.T) {
	item := store.MediaItem{
		Container:   "mkv",
		VideoCodec:  "hevc",
		AudioCodec:  "truehd",
		VideoWidth:  3840,
		VideoHeight: 2160,
		Bitrate:     40_000_000,
	}

	// Native TV app supporting everything -> direct play.
	mode, _ := choosePlaybackMode(item, ClientCapabilities{
		Containers:  []string{"mkv", "mp4"},
		VideoCodecs: []string{"hevc", "h264"},
		AudioCodecs: []string{"truehd", "eac3", "aac"},
	}, true)
	if mode != "direct" {
		t.Fatalf("expected direct, got %s", mode)
	}

	// Browser supporting HEVC video but only MP4/AAC -> remux (video copy).
	mode, _ = choosePlaybackMode(item, ClientCapabilities{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"hevc", "h264"},
		AudioCodecs: []string{"aac"},
	}, true)
	if mode != "remux" {
		t.Fatalf("expected remux, got %s", mode)
	}

	// Browser supporting only H.264 1080p -> transcode.
	mode, _ = choosePlaybackMode(item, ClientCapabilities{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		MaxHeight:   1080,
	}, true)
	if mode != "transcode" {
		t.Fatalf("expected transcode, got %s", mode)
	}

	// Fallback to direct when ffmpeg is unavailable.
	mode, _ = choosePlaybackMode(item, ClientCapabilities{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
	}, false)
	if mode != "direct" {
		t.Fatalf("expected direct fallback when ffmpeg unavailable, got %s", mode)
	}
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

func TestEvaluatePlayback_4KRemux_10BitAnime_HDR_And_BurnIn(t *testing.T) {
	// 1. 4K HEVC 10-bit HDR10 MKV with AAC stereo -> zero-transcode fMP4 remux (video copy + audio copy)
	// when client supports 10-bit HEVC + HDR + AAC.
	item4K := store.MediaItem{
		Container:     "mkv",
		VideoCodec:    "hevc",
		VideoWidth:    3840,
		VideoHeight:   2160,
		VideoHDR:      "hdr10",
		AudioCodec:    "aac",
		AudioChannels: 2,
		Bitrate:       55_000_000,
	}
	streams4K := []metadata.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Width: 3840, Height: 2160, HDR: "hdr10"},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
		{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng"},
	}
	d := evaluatePlayback(item4K, streams4K, &AudioTrackDTO{Index: 1, Codec: "aac", Channels: 2}, ClientCapabilities{
		Containers:        []string{"mp4", "webm"},
		VideoCodecs:       []string{"h264", "hevc"},
		AudioCodecs:       []string{"aac", "opus"},
		Supports10BitHEVC: boolPtr(true),
		SupportsHDR:       boolPtr(true),
		MaxAudioChannels:  2,
	}, true)
	if d.mode != "remux" || !d.videoCopy || !d.audioCopy {
		t.Fatalf("expected zero-transcode remux for 4K HEVC+AAC on HDR-capable browser, got %+v", d)
	}

	// 2. Same 4K HDR10 MKV on an SDR-only display -> must transcode to SDR.
	d = evaluatePlayback(item4K, streams4K, &AudioTrackDTO{Index: 1, Codec: "aac", Channels: 2}, ClientCapabilities{
		Containers:        []string{"mp4"},
		VideoCodecs:       []string{"h264", "hevc"},
		AudioCodecs:       []string{"aac"},
		Supports10BitHEVC: boolPtr(true),
		SupportsHDR:       boolPtr(false),
	}, true)
	if d.mode != "transcode" || d.videoCopy {
		t.Fatalf("expected transcode for HDR10 on SDR-only client, got %+v", d)
	}

	// 3. 10-bit H.264 High 10 anime MKV -> must transcode for browser (supports_10bit_h264=false).
	anime10Bit := store.MediaItem{
		Container:     "mkv",
		VideoCodec:    "h264",
		VideoWidth:    1920,
		VideoHeight:   1080,
		AudioCodec:    "flac",
		AudioChannels: 2,
	}
	animeStreams := []metadata.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Profile: "High 10"},
		{Index: 1, Type: "audio", Codec: "flac", Channels: 2},
	}
	d = evaluatePlayback(anime10Bit, animeStreams, &AudioTrackDTO{Index: 1, Codec: "flac", Channels: 2}, ClientCapabilities{
		Containers:        []string{"mp4"},
		VideoCodecs:       []string{"h264"},
		AudioCodecs:       []string{"aac", "flac"},
		Supports10BitH264: boolPtr(false),
	}, true)
	if d.mode != "transcode" || d.videoCopy || d.audioCopy {
		t.Fatalf("expected transcode + AAC encode for 10-bit H.264+FLAC anime, got %+v", d)
	}

	// 4. Dolby Vision MKV on non-DoVi browser -> must transcode.
	doviItem := store.MediaItem{
		Container:   "mkv",
		VideoCodec:  "hevc",
		VideoWidth:  3840,
		VideoHeight: 2160,
		VideoHDR:    "dovi",
		AudioCodec:  "eac3",
	}
	d = evaluatePlayback(doviItem, nil, nil, ClientCapabilities{
		Containers:        []string{"mp4"},
		VideoCodecs:       []string{"hevc"},
		AudioCodecs:       []string{"aac", "eac3"},
		Supports10BitHEVC: boolPtr(true),
		SupportsHDR:       boolPtr(true),
		SupportsDoVi:      boolPtr(false),
	}, true)
	if d.mode != "transcode" || d.videoCopy {
		t.Fatalf("expected transcode for Dolby Vision on non-DoVi client, got %+v", d)
	}

	// 5. Bitmap PGS subtitle burn-in forces transcode even for Direct Playable H.264 MP4.
	mp4Item := store.MediaItem{
		Container:  "mp4",
		VideoCodec: "h264",
		AudioCodec: "aac",
	}
	d = evaluatePlayback(mp4Item, nil, nil, ClientCapabilities{
		Containers:            []string{"mp4"},
		VideoCodecs:           []string{"h264"},
		AudioCodecs:           []string{"aac"},
		BurnSubtitleStreamIdx: intPtr(2),
	}, true)
	if d.mode != "transcode" || d.videoCopy || !d.audioCopy {
		t.Fatalf("expected transcode with audio copy for PGS burn-in, got %+v", d)
	}

	// 6. Legacy MPEG-2 video cannot be remuxed into fMP4 HLS -> forces transcode.
	mpeg2Item := store.MediaItem{
		Container:  "mkv",
		VideoCodec: "mpeg2video",
		AudioCodec: "ac3",
	}
	d = evaluatePlayback(mpeg2Item, nil, nil, ClientCapabilities{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"mpeg2video", "h264"},
		AudioCodecs: []string{"aac"},
	}, true)
	if d.mode != "transcode" {
		t.Fatalf("expected transcode for non-fMP4 codec mpeg2video, got %+v", d)
	}
}

func TestBuildFFmpegArgs_fMP4_HEVC_HDR_And_BurnIn(t *testing.T) {
	tr := &Transcoder{
		ffmpegBin:        "/usr/bin/ffmpeg",
		hwAccel:          HWAccelNone,
		hasZscaleTonemap: true,
	}

	// 4K HEVC MKV remux with audio copy -> fMP4 + hvc1 tag + -c:v copy + -c:a copy + -sn -dn.
	remuxArgs := strings.Join(tr.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:            "remux",
		VideoCodec:      "hevc",
		AudioCopy:       true,
		StartPositionMS: 120_000,
	}), " ")
	for _, want := range []string{
		"-ss 120.000",
		"-c:v copy",
		"-tag:v hvc1",
		"-c:a copy",
		"-sn -dn",
		"-hls_segment_type fmp4",
		"-hls_fmp4_init_filename init.mp4",
		"independent_segments+temp_file",
		"seg_%05d.m4s",
	} {
		if !strings.Contains(remuxArgs, want) {
			t.Fatalf("expected remux args to contain %q, got: %s", want, remuxArgs)
		}
	}

	// HDR10 transcode with PGS burn-in -> tone-map + overlay + yuv420p + libx264 + keyframe alignment.
	burnArgs := strings.Join(tr.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:                  "transcode",
		VideoCodec:            "hevc",
		VideoHDR:              "hdr10",
		MaxHeight:             1080,
		MaxBitrate:            10_000_000,
		BurnSubtitleStreamIdx: intPtr(3),
	}), " ")
	for _, want := range []string{
		"-filter_complex",
		"tonemap=tonemap=hable:desat=0",
		"[0:3]overlay=eof_action=pass",
		"format=yuv420p",
		"-c:v libx264",
		"-profile:v high",
		"-maxrate 10000k",
		"-c:a aac",
	} {
		if !strings.Contains(burnArgs, want) {
			t.Fatalf("expected burn-in transcode args to contain %q, got: %s", want, burnArgs)
		}
	}

	// Dolby Vision with libplacebo available uses open-source libplacebo DV tone-mapping (fixes Profile 5).
	trDV := &Transcoder{ffmpegBin: "/usr/bin/ffmpeg", hwAccel: HWAccelNone, hasZscaleTonemap: true, hasLibplacebo: true}
	dvArgs := strings.Join(trDV.buildFFmpegArgs("/media/dv5.mkv", "/tmp/out", TranscodeOptions{
		Mode:     "transcode",
		VideoHDR: "dovi",
	}), " ")
	if !strings.Contains(dvArgs, "libplacebo=tonemapping=bt.2390") || !strings.Contains(dvArgs, "apply_dolbyvision=true") {
		t.Fatalf("expected libplacebo DV tone-map in args, got: %s", dvArgs)
	}
	// When DisableLibplacebo is set (fallback), falls back cleanly to zscale/tonemap.
	dvFallbackArgs := strings.Join(trDV.buildFFmpegArgs("/media/dv5.mkv", "/tmp/out", TranscodeOptions{
		Mode:              "transcode",
		VideoHDR:          "dovi",
		DisableLibplacebo: true,
	}), " ")
	if strings.Contains(dvFallbackArgs, "libplacebo") || !strings.Contains(dvFallbackArgs, "tonemap=tonemap=hable") {
		t.Fatalf("expected zscale/tonemap fallback when DisableLibplacebo=true, got: %s", dvFallbackArgs)
	}

	// VideoToolbox hardware encoder uses -q:v 65 and -allow_sw 1 when no MaxBitrate is set.
	trVT := &Transcoder{ffmpegBin: "/usr/bin/ffmpeg", hwAccel: HWAccelVideoToolbox}
	vtArgs := strings.Join(trVT.buildFFmpegArgs("/media/movie.mkv", "/tmp/out", TranscodeOptions{
		Mode: "transcode",
	}), " ")
	for _, want := range []string{"-c:v h264_videotoolbox", "-allow_sw 1", "-q:v 65", "format=yuv420p"} {
		if !strings.Contains(vtArgs, want) {
			t.Fatalf("expected VideoToolbox args to contain %q, got: %s", want, vtArgs)
		}
	}

	// fMP4 allowed file regex accepts init.mp4 and seg_00001.m4s, rejects traversal.
	for _, valid := range []string{"index.m3u8", "init.mp4", "seg_00000.m4s", "seg_12345.ts"} {
		if !reAllowedHLSFile.MatchString(valid) {
			t.Fatalf("expected %q to match reAllowedHLSFile", valid)
		}
	}
	for _, invalid := range []string{"../init.mp4", "seg_1.m4s", "other.mp4"} {
		if reAllowedHLSFile.MatchString(invalid) {
			t.Fatalf("expected %q to be rejected by reAllowedHLSFile", invalid)
		}
	}
}

func TestBuildSubtitleCatalog_IncludesBitmapBurnInAndDoesNotAutoSelect(t *testing.T) {
	streams := []metadata.StreamInfo{
		{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "eng", Title: "English PGS"},
		{Index: 3, Type: "subtitle", Codec: "subrip", Language: "eng"},
	}
	subs := buildSubtitleCatalog(nil, streams, true, "tok")
	if len(subs) != 2 {
		t.Fatalf("expected 2 subtitle tracks, got %d", len(subs))
	}
	if !subs[0].RequiresBurnIn || subs[0].Source != "burn_in" || subs[0].StreamIndex == nil || *subs[0].StreamIndex != 2 {
		t.Fatalf("unexpected PGS burn-in DTO: %+v", subs[0])
	}
	if subs[1].RequiresBurnIn || subs[1].Source != "embedded" || subs[1].URL == "" {
		t.Fatalf("unexpected embedded text DTO: %+v", subs[1])
	}

	// Auto-selection must pick text subtitle, never burn-in PGS.
	pgsOnly := []SubtitleStreamDTO{subs[0]}
	if sel := selectPreferredSubtitle(pgsOnly, "ja", store.UserPreferences{SubtitlesEnabled: true}, true); sel != nil {
		t.Fatalf("expected burn-in PGS track not to be auto-selected, got %v", *sel)
	}
}
