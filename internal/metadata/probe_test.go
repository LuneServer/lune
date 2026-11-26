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
