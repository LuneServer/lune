package stream

import (
	"testing"

	"github.com/LuneServer/lune/internal/store"
)

func TestFallbackSkipsSubtitlesInTheAudioLanguage(t *testing.T) {
	subs := []SubtitleStreamDTO{{Index: 0, Language: "ko"}, {Index: 1, Language: ""}}
	prefs := store.UserPreferences{SubtitleLanguage: "en"}
	if sel := selectPreferredSubtitle(subs, "ko", prefs, false); sel == nil || *sel != 1 {
		t.Fatalf("expected the untagged track, got %v", sel)
	}
	if sel := selectPreferredSubtitle(subs[:1], "ko", prefs, false); sel != nil {
		t.Fatalf("expected no subtitles when only Korean ones exist, got %v", *sel)
	}
	prefs.SubtitlesEnabled = true
	if sel := selectPreferredSubtitle(subs[:1], "ko", prefs, false); sel == nil || *sel != 0 {
		t.Fatalf("always-on subtitles should still take the Korean track, got %v", sel)
	}
}

func TestAnimeAudioSelection(t *testing.T) {
	audio := []AudioTrackDTO{
		{Index: 0, Language: "ja", Default: true},
		{Index: 1, Language: "en"},
	}
	jaOnly := []AudioTrackDTO{{Index: 0, Language: "ja", Default: true}}
	enOnly := []AudioTrackDTO{{Index: 0, Language: "en", Default: true}}
	subs := []SubtitleStreamDTO{
		{Index: 0, Language: "en", Forced: true},
		{Index: 1, Language: "en"},
	}
	const signs, full = 0, 1

	// The goal setup: subtitles off for normal media, anime subbed.
	subbed := store.UserPreferences{SubtitleLanguage: "en", AnimeAudio: "subbed"}
	dubbed := store.UserPreferences{SubtitleLanguage: "en", AnimeAudio: "dubbed"}

	cases := []struct {
		name      string
		tracks    []AudioTrackDTO
		prefs     store.UserPreferences
		anime     bool
		wantAudio int
		wantSub   int // -1 = none
	}{
		{"anime subbed", audio, subbed, true, 0, full},
		{"anime subbed ignores global en audio", audio, store.UserPreferences{AudioLanguage: "en", SubtitleLanguage: "en", AnimeAudio: "subbed"}, true, 0, full},
		{"anime subbed with only an English track", enOnly, subbed, true, 0, signs},
		{"anime dubbed", audio, dubbed, true, 1, signs},
		{"anime dubbed ignores subtitles_enabled", audio, store.UserPreferences{SubtitleLanguage: "en", SubtitlesEnabled: true, AnimeAudio: "dubbed"}, true, 1, signs},
		{"anime dubbed with old ja audio pref", audio, store.UserPreferences{AudioLanguage: "ja", SubtitleLanguage: "en", AnimeAudio: "dubbed"}, true, 1, signs},
		{"anime dubbed without a dub keeps full subs", jaOnly, dubbed, true, 0, full},
		{"movie not affected by subbed", enOnly, subbed, false, 0, signs},
		{"movie subtitles_enabled still works", enOnly, store.UserPreferences{SubtitleLanguage: "en", SubtitlesEnabled: true, AnimeAudio: "dubbed"}, false, 0, full},
		{"foreign movie turns on full subtitles", audio, dubbed, false, 0, full},
		{"foreign movie with English audio preference", audio, store.UserPreferences{AudioLanguage: "en", SubtitleLanguage: "en"}, false, 1, signs},
		{"untagged audio leaves subtitles off", []AudioTrackDTO{{Index: 0, Language: "und", Default: true}}, store.UserPreferences{SubtitleLanguage: "en"}, false, 0, signs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aIdx, aLang := selectPreferredAudio(tc.tracks, nil, tc.prefs, tc.anime, true)
			if aIdx == nil || *aIdx != tc.wantAudio {
				t.Fatalf("audio = %v, want %d", aIdx, tc.wantAudio)
			}
			sIdx := selectPreferredSubtitle(subs, aLang, tc.prefs, tc.anime)
			got := -1
			if sIdx != nil {
				got = *sIdx
			}
			if got != tc.wantSub {
				t.Fatalf("subtitle = %d, want %d", got, tc.wantSub)
			}
		})
	}
}
