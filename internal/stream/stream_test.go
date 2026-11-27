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

	// 4b. Dolby Vision Profile 8 (dovi_hdr10) on HDR10-capable non-DoVi client -> zero-transcode remux to HDR10 hvc1!
	doviP8Item := store.MediaItem{
		Container:   "mkv",
		VideoCodec:  "hevc",
		VideoWidth:  3840,
		VideoHeight: 2160,
		VideoHDR:    "dovi_hdr10",
		AudioCodec:  "eac3",
	}
	doviP8Streams := []metadata.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", PixFmt: "yuv420p10le", HDR: "dovi_hdr10", DVProfile: 8, DVCompatID: 1},
	}
	d = evaluatePlayback(doviP8Item, doviP8Streams, nil, ClientCapabilities{
		Containers:        []string{"mp4"},
		VideoCodecs:       []string{"hevc"},
		AudioCodecs:       []string{"aac", "eac3"},
		Supports10BitHEVC: boolPtr(true),
		SupportsHDR:       boolPtr(true),
		SupportsDoVi:      boolPtr(false),
	}, true)
	if d.mode != "remux" || !d.videoCopy {
		t.Fatalf("expected remux (video copy) for Dolby Vision Profile 8 on HDR10-capable client, got %+v", d)
	}

	// 4c. MP4 with HEVC hev1 tag -> forces remux (-tag:v hvc1) instead of Direct Play.
	hev1MP4 := store.MediaItem{
		Container:  "mp4",
		VideoCodec: "hevc",
		AudioCodec: "aac",
	}
	hev1Streams := []metadata.StreamInfo{
		{Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", PixFmt: "yuv420p10le", CodecTag: "hev1"},
	}
	d = evaluatePlayback(hev1MP4, hev1Streams, nil, ClientCapabilities{
		Containers:        []string{"mp4"},
		VideoCodecs:       []string{"hevc"},
		AudioCodecs:       []string{"aac"},
		Supports10BitHEVC: boolPtr(true),
	}, true)
	if d.mode != "remux" || !d.videoCopy || !d.audioCopy {
		t.Fatalf("expected zero-transcode remux for hev1-tagged HEVC MP4, got %+v", d)
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

	// 4K HEVC MKV remux with audio copy -> fMP4 + hvc1 tag + -noaccurate_seek -ss + -c:v copy + -c:a copy + -sn -dn.
	remuxArgs := strings.Join(tr.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:            "remux",
		VideoCodec:      "hevc",
		AudioCopy:       true,
		StartPositionMS: 120_000,
	}), " ")
	for _, want := range []string{
		"-noaccurate_seek -ss 120.000",
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

	// Remux with audio transcode must omit first_pts=0 and use async=1000 + negative_cts_offsets so A/V stay locked.
	remuxAACArgs := strings.Join(tr.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:            "remux",
		VideoCodec:      "hevc",
		AudioCopy:       false,
		StartPositionMS: 600_000,
	}), " ")
	if !strings.Contains(remuxAACArgs, "-noaccurate_seek -ss 600.000") {
		t.Fatalf("expected remux AAC args to use -noaccurate_seek -ss 600.000, got: %s", remuxAACArgs)
	}
	if !strings.Contains(remuxAACArgs, "-af aresample=async=1000 ") || strings.Contains(remuxAACArgs, "first_pts=0") {
		t.Fatalf("expected remux AAC args to use aresample=async=1000 without first_pts=0, got: %s", remuxAACArgs)
	}
	if !strings.Contains(remuxAACArgs, "-hls_segment_options movflags=+negative_cts_offsets") {
		t.Fatalf("expected negative_cts_offsets in fMP4 HLS args, got: %s", remuxAACArgs)
	}

	// HDR10 transcode with PGS burn-in -> tone-map + scale2ref + overlay + yuv420p + BT.709 color tags + libx264 + keyframe alignment.
	burnArgs := strings.Join(tr.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:                  "transcode",
		VideoCodec:            "hevc",
		VideoHDR:              "hdr10",
		MaxHeight:             1080,
		MaxBitrate:            10_000_000,
		BurnSubtitleStreamIdx: intPtr(3),
	}), " ")
	for _, want := range []string{
		"-fflags +genpts",
		"-filter_complex",
		"tonemap=tonemap=hable:desat=0",
		"[0:3][vpre]scale2ref[sub_scaled][vbase];[vbase][sub_scaled]overlay=eof_action=pass",
		"setsar=1",
		"format=yuv420p",
		"-c:v libx264",
		"-profile:v high",
		"-maxrate 10000k",
		"-color_primaries bt709",
		"-color_trc bt709",
		"-colorspace bt709",
		"-color_range tv",
		"-fps_mode passthrough",
		"-c:a aac",
		"-af aresample=async=1000",
		"-hls_segment_options movflags=+negative_cts_offsets",
	} {
		if !strings.Contains(burnArgs, want) {
			t.Fatalf("expected burn-in transcode args to contain %q, got: %s", want, burnArgs)
		}
	}
	if strings.Contains(burnArgs, "first_pts=0") {
		t.Fatalf("expected transcode args to omit first_pts=0, got: %s", burnArgs)
	}

	// HDR10 transcode without zscale/libplacebo falls back to BT.2020->BT.709 matrix conversion + BT.709 SDR tags.
	trNoZscale := &Transcoder{ffmpegBin: "/usr/bin/ffmpeg", hwAccel: HWAccelNone}
	fallbackHDRArgs := strings.Join(trNoZscale.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:       "transcode",
		VideoCodec: "hevc",
		VideoHDR:   "hdr10",
	}), " ")
	for _, want := range []string{
		"scale=in_color_matrix=bt2020:out_color_matrix=bt709",
		"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv",
		"-color_primaries bt709",
		"-color_trc bt709",
		"-colorspace bt709",
	} {
		if !strings.Contains(fallbackHDRArgs, want) {
			t.Fatalf("expected fallback HDR args to contain %q, got: %s", want, fallbackHDRArgs)
		}
	}

	// VA-API full GPU decode + scale_vaapi/tonemap_vaapi pipeline on custom render node.
	trVAAPI := &Transcoder{
		ffmpegBin:       "/usr/bin/ffmpeg",
		hwAccel:         HWAccelVAAPI,
		vaapiDevice:     "/dev/dri/renderD129",
		hasTonemapVAAPI: true,
		hasScaleVAAPI:   true,
	}
	vaapiHDRArgs := strings.Join(trVAAPI.buildFFmpegArgs("/media/4k.mkv", "/tmp/out", TranscodeOptions{
		Mode:         "transcode",
		VideoCodec:   "hevc",
		VideoHDR:     "hdr10",
		SourceHeight: 2160,
		MaxHeight:    1080,
	}), " ")
	for _, want := range []string{
		"-hwaccel vaapi",
		"-hwaccel_device /dev/dri/renderD129",
		"-hwaccel_output_format vaapi",
		"tonemap_vaapi=format=nv12:matrix=bt709:primaries=bt709:transfer=bt709,scale_vaapi=w=-2:h=1080",
		"-c:v h264_vaapi",
	} {
		if !strings.Contains(vaapiHDRArgs, want) {
			t.Fatalf("expected VAAPI HDR args to contain %q, got: %s", want, vaapiHDRArgs)
		}
	}

	// 10-bit H.264 (Hi10P) cannot be HW-decoded on VA-API -> must use CPU decode + format=nv12,hwupload + h264_vaapi.
	vaapiHi10PArgs := strings.Join(trVAAPI.buildFFmpegArgs("/media/anime.mkv", "/tmp/out", TranscodeOptions{
		Mode:         "transcode",
		VideoCodec:   "h264",
		VideoProfile: "High 10",
		VideoPixFmt:  "yuv420p10le",
	}), " ")
	if strings.Contains(vaapiHi10PArgs, "-hwaccel vaapi") {
		t.Fatalf("expected 10-bit H.264 not to use VA-API HW decode, got: %s", vaapiHi10PArgs)
	}
	for _, want := range []string{"-vaapi_device /dev/dri/renderD129", "format=nv12,hwupload", "-c:v h264_vaapi"} {
		if !strings.Contains(vaapiHi10PArgs, want) {
			t.Fatalf("expected VAAPI Hi10P args to contain %q, got: %s", want, vaapiHi10PArgs)
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

	// fMP4 allowed file regex accepts master.m3u8, index.m3u8, init.mp4 and seg_00001.m4s, rejects traversal.
	for _, valid := range []string{"master.m3u8", "index.m3u8", "init.mp4", "seg_00000.m4s", "seg_12345.ts"} {
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

func TestVODPlaylistAndMasterPlaylist(t *testing.T) {
	// 10s remaining -> 4 segments (3s, 3s, 3s, 1s), VOD type, #EXT-X-ENDLIST.
	pl := buildVODPlaylist(10_000)
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MAP:URI=\"init.mp4\"",
		"#EXTINF:3.000000,\nseg_00000.m4s",
		"#EXTINF:3.000000,\nseg_00002.m4s",
		"#EXTINF:1.000000,\nseg_00003.m4s",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(pl, want) {
			t.Fatalf("expected VOD playlist to contain %q, got:\n%s", want, pl)
		}
	}

	// E-AC-3 Atmos remux master playlist declares ec-3 and CHANNELS="16/JOC".
	master := buildMasterPlaylist(TranscodeOptions{
		Mode:          "remux",
		VideoCodec:    "hevc",
		VideoProfile:  "Main 10",
		VideoHDR:      "dovi_hdr10",
		VideoWidth:    3840,
		SourceHeight:  2160,
		Bitrate:       25_000_000,
		AudioCopy:     true,
		AudioCodec:    "eac3",
		AudioProfile:  "Dolby Digital Plus + Dolby Atmos",
		AudioChannels: 6,
		AudioLanguage: "en",
		AudioTitle:    "English Atmos",
	})
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English Atmos",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="16/JOC"`,
		`CODECS="hvc1.2.4.L153.B0,ec-3",AUDIO="audio"`,
		`RESOLUTION=3840x2160`,
		"\nindex.m3u8\n",
	} {
		if !strings.Contains(master, want) {
			t.Fatalf("expected master playlist to contain %q, got:\n%s", want, master)
		}
	}

	// Remux mode (-c:v copy) must NOT use synthetic 3s VOD playlist (variable source GOPs),
	// whereas transcode mode (forced 3s keyframes) uses VOD playlist + ffmpeg.m3u8.
	if usesVODPlaylist(TranscodeOptions{Mode: "remux", DurationMS: 3_600_000}) {
		t.Fatalf("expected usesVODPlaylist to be false for remux mode")
	}
	if !usesVODPlaylist(TranscodeOptions{Mode: "transcode", DurationMS: 3_600_000}) {
		t.Fatalf("expected usesVODPlaylist to be true for transcode mode")
	}

	// Seek-restarted VOD FFmpeg args include -output_ts_offset, -start_number, and ffmpeg.m3u8.
	tr := &Transcoder{ffmpegBin: "/usr/bin/ffmpeg", hwAccel: HWAccelNone}
	seekArgs := strings.Join(tr.buildFFmpegArgs("/media/movie.mkv", "/tmp/out", TranscodeOptions{
		Mode:            "transcode",
		VideoCodec:      "hevc",
		AudioCopy:       true,
		DurationMS:      3_600_000,
		StartPositionMS: 900_000,
		StartSegment:    300,
	}), " ")
	for _, want := range []string{
		"-output_ts_offset 900.000",
		"-start_number 300",
		"/tmp/out/ffmpeg.m3u8",
	} {
		if !strings.Contains(seekArgs, want) {
			t.Fatalf("expected VOD seek args to contain %q, got: %s", want, seekArgs)
		}
	}
}

func TestVAAPIHybridHDRAndDownscaleBeforeTonemap(t *testing.T) {
	// VA-API GPU without tonemap_vaapi (e.g. Intel Gen <= 10 or AMD Mesa):
	// keeps GPU decode + GPU downscale via scale_vaapi BEFORE hwdownload + CPU zscale/tonemap.
	trHybrid := &Transcoder{
		ffmpegBin:        "/usr/bin/ffmpeg",
		hwAccel:          HWAccelVAAPI,
		vaapiDevice:      "/dev/dri/renderD128",
		hasTonemapVAAPI:  false,
		hasScaleVAAPI:    true,
		hasZscaleTonemap: true,
	}
	hybridArgs := strings.Join(trHybrid.buildFFmpegArgs("/media/4k_hdr.mkv", "/tmp/out", TranscodeOptions{
		Mode:         "transcode",
		VideoCodec:   "hevc",
		VideoHDR:     "hdr10",
		SourceHeight: 2160,
		MaxHeight:    1080,
	}), " ")
	for _, want := range []string{
		"-hwaccel vaapi",
		"-hwaccel_output_format vaapi",
		"scale_vaapi=w=-2:h=1080,hwdownload,format=p010,zscale=t=linear:npl=100",
		"format=nv12,hwupload",
		"-c:v h264_vaapi",
	} {
		if !strings.Contains(hybridArgs, want) {
			t.Fatalf("expected hybrid VA-API HDR args to contain %q, got: %s", want, hybridArgs)
		}
	}

	// CPU fallback HDR transcode with downscale puts scale BEFORE zscale/tonemap.
	trCPU := &Transcoder{
		ffmpegBin:        "/usr/bin/ffmpeg",
		hwAccel:          HWAccelNone,
		hasZscaleTonemap: true,
	}
	cpuHDRArgs := strings.Join(trCPU.buildFFmpegArgs("/media/4k_hdr.mkv", "/tmp/out", TranscodeOptions{
		Mode:         "transcode",
		VideoCodec:   "hevc",
		VideoHDR:     "hdr10",
		SourceHeight: 2160,
		MaxHeight:    1080,
	}), " ")
	scaleIdx := strings.Index(cpuHDRArgs, "scale=trunc(oh*dar/2)*2:min(ih\\,1080),setsar=1")
	zscaleIdx := strings.Index(cpuHDRArgs, "zscale=t=linear:npl=100")
	if scaleIdx < 0 || zscaleIdx < 0 || scaleIdx > zscaleIdx {
		t.Fatalf("expected scale before zscale/tonemap in CPU HDR fallback, got: %s", cpuHDRArgs)
	}

	// VA-API with verified tonemap_vaapi prefers tonemap_vaapi over libplacebo for dovi_hdr10.
	trVAAPIDV := &Transcoder{
		ffmpegBin:       "/usr/bin/ffmpeg",
		hwAccel:         HWAccelVAAPI,
		vaapiDevice:     "/dev/dri/renderD128",
		hasTonemapVAAPI: true,
		hasScaleVAAPI:   true,
		hasLibplacebo:   true,
	}
	dvHDR10Args := strings.Join(trVAAPIDV.buildFFmpegArgs("/media/dv8.mkv", "/tmp/out", TranscodeOptions{
		Mode:         "transcode",
		VideoCodec:   "hevc",
		VideoHDR:     "dovi_hdr10",
		DVProfile:    8,
		DVCompatID:   1,
		SourceHeight: 2160,
		MaxHeight:    1080,
	}), " ")
	if strings.Contains(dvHDR10Args, "libplacebo") || !strings.Contains(dvHDR10Args, "tonemap_vaapi=") {
		t.Fatalf("expected VA-API with tonemap_vaapi to prefer tonemap_vaapi for dovi_hdr10, got: %s", dvHDR10Args)
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

func TestParseFrameCRCStartMS(t *testing.T) {
	// Standard 1/1000 MKV with B-frames (DTS < PTS): returns DTS so offset + video.currentTime == exact PTS.
	mkvOut := strings.Join([]string{
		"#extradata 0:      152, 0x56812563",
		"#tb 0: 1/1000",
		"#media_type 0: video",
		"0,     598875,     599042,       42,   272191, 0xbf323b9a",
	}, "\n")
	got, ok := parseFrameCRCStartMS(mkvOut)
	if !ok || got != 598875 {
		t.Fatalf("expected 598875, got %d (ok=%v)", got, ok)
	}

	// Non-millisecond timebase (1/90000 MPEG-TS): 54,000,000 ticks == 600,000ms.
	tsOut := "#tb 0: 1/90000\n0,   54000000,   54007500,     3750,   123456, 0xdeadbeef\n"
	got, ok = parseFrameCRCStartMS(tsOut)
	if !ok || got != 600000 {
		t.Fatalf("expected 600000, got %d (ok=%v)", got, ok)
	}

	// Missing/negative DTS falls back to PTS.
	ptsFallback := "#tb 0: 1/1000\n0,       -167,      10000,       42,    12345, 0x12345678\n"
	got, ok = parseFrameCRCStartMS(ptsFallback)
	if !ok || got != 10000 {
		t.Fatalf("expected 10000 fallback to PTS, got %d (ok=%v)", got, ok)
	}

	// Empty output returns false.
	if _, ok := parseFrameCRCStartMS(""); ok {
		t.Fatalf("expected ok=false for empty framecrc output")
	}
}
