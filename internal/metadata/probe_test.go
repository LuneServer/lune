package metadata

import "testing"

func TestParseFFProbeJSON_HDR10AndStreams(t *testing.T) {
	sample := []byte(`{
		"format": {
			"format_name": "matroska,webm",
			"duration": "10140.5",
			"bit_rate": "45200000"
		},
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"profile": "Main 10",
				"width": 3840,
				"height": 2160,
				"color_transfer": "smpte2084",
				"color_primaries": "bt2020",
				"disposition": { "default": 1 }
			},
			{
				"index": 1,
				"codec_type": "audio",
				"codec_name": "truehd",
				"channels": 8,
				"channel_layout": "7.1",
				"tags": { "language": "eng", "title": "Atmos 7.1" },
				"disposition": { "default": 1 }
			},
			{
				"index": 2,
				"codec_type": "subtitle",
				"codec_name": "subrip",
				"tags": { "language": "eng", "title": "English" },
				"disposition": { "forced": 0 }
			}
		]
	}`)

	res, err := parseFFProbeJSON(sample, "/media/movies/Interstellar (2014).mkv")
	if err != nil {
		t.Fatalf("parseFFProbeJSON: %v", err)
	}
	if res.Container != "mkv" || res.VideoCodec != "hevc" || res.VideoWidth != 3840 || res.VideoHDR != "hdr10" {
		t.Fatalf("unexpected video summary: %+v", res)
	}
	if res.AudioCodec != "truehd" || res.AudioChannels != 8 {
		t.Fatalf("unexpected audio summary: %+v", res)
	}
	if res.DurationMS != 10140500 || len(res.Streams) != 3 {
		t.Fatalf("unexpected duration/streams: dur=%d streams=%d", res.DurationMS, len(res.Streams))
	}
}

func TestParseFFProbeJSON_AnimeChaptersAndDualAudio(t *testing.T) {
	sample := []byte(`{
		"format": { "format_name": "matroska,webm", "duration": "1440.0", "bit_rate": "8500000" },
		"streams": [
			{ "index": 0, "codec_type": "video", "codec_name": "hevc", "width": 1920, "height": 1080 },
			{ "index": 1, "codec_type": "audio", "codec_name": "flac", "channels": 2, "tags": { "language": "jpn", "title": "Japanese" }, "disposition": { "default": 1 } },
			{ "index": 2, "codec_type": "audio", "codec_name": "aac", "channels": 2, "tags": { "language": "eng", "title": "English Dub" } },
			{ "index": 3, "codec_type": "subtitle", "codec_name": "ass", "tags": { "language": "eng", "title": "Full Subtitles" }, "disposition": { "default": 1 } }
		],
		"chapters": [
			{ "id": 0, "start_time": "0.000", "end_time": "90.000", "tags": { "title": "OP" } },
			{ "id": 1, "start_time": "90.000", "end_time": "1320.000", "tags": { "title": "Part A" } },
			{ "id": 2, "start_time": "1320.000", "end_time": "1410.000", "tags": { "title": "Ending Theme" } },
			{ "id": 3, "start_time": "1410.000", "end_time": "1440.000", "tags": { "title": "Next Episode Preview" } }
		]
	}`)

	res, err := parseFFProbeJSON(sample, "/media/anime/Frieren - 01.mkv")
	if err != nil {
		t.Fatalf("parseFFProbeJSON: %v", err)
	}
	langs := ExtractAudioLanguages(res.Streams)
	if len(langs) != 2 || langs[0] != "ja" || langs[1] != "en" {
		t.Fatalf("unexpected audio languages: %+v", langs)
	}
	if len(res.Chapters) != 4 {
		t.Fatalf("expected 4 chapters, got %d", len(res.Chapters))
	}
	wantKinds := []string{"intro", "chapter", "outro", "preview"}
	for i, wk := range wantKinds {
		if res.Chapters[i].Kind != wk {
			t.Fatalf("chapter %d kind = %q, want %q", i, res.Chapters[i].Kind, wk)
		}
	}
}

func TestParseFFProbeJSON_DolbyVisionProfilesAndCodecTag(t *testing.T) {
	// Profile 8 with bl_compat=1 -> dovi_hdr10 + codec_tag_string + pix_fmt.
	sampleP8 := []byte(`{
		"format": { "format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "7200.0", "bit_rate": "25000000" },
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"codec_tag_string": "hev1",
				"profile": "Main 10",
				"pix_fmt": "yuv420p10le",
				"width": 3840,
				"height": 2160,
				"color_space": "bt2020nc",
				"color_transfer": "smpte2084",
				"color_primaries": "bt2020",
				"side_data_list": [
					{ "side_data_type": "DOVI configuration record", "dv_profile": 8, "dv_bl_signal_compatibility_id": 1 }
				]
			}
		]
	}`)
	res, err := parseFFProbeJSON(sampleP8, "/media/movies/DV8.mp4")
	if err != nil {
		t.Fatalf("parseFFProbeJSON P8: %v", err)
	}
	if res.VideoHDR != "dovi_hdr10" || len(res.Streams) != 1 {
		t.Fatalf("expected dovi_hdr10, got %+v", res)
	}
	st := res.Streams[0]
	if st.PixFmt != "yuv420p10le" || st.CodecTag != "hev1" || st.DVProfile != 8 || st.DVCompatID != 1 {
		t.Fatalf("unexpected stream metadata: %+v", st)
	}

	// Profile 5 (no HDR10 base layer) -> dovi.
	sampleP5 := []byte(`{
		"format": { "format_name": "matroska,webm", "duration": "3600.0" },
		"streams": [
			{
				"index": 0,
				"codec_type": "video",
				"codec_name": "hevc",
				"profile": "Main 10",
				"pix_fmt": "yuv420p10le",
				"width": 3840,
				"height": 2160,
				"side_data_list": [
					{ "side_data_type": "DOVI configuration record", "dv_profile": 5, "dv_bl_signal_compatibility_id": 0 }
				]
			}
		]
	}`)
	res5, err := parseFFProbeJSON(sampleP5, "/media/movies/DV5.mkv")
	if err != nil {
		t.Fatalf("parseFFProbeJSON P5: %v", err)
	}
	if res5.VideoHDR != "dovi" || res5.Streams[0].DVProfile != 5 {
		t.Fatalf("expected dovi profile 5, got %+v", res5)
	}
}
