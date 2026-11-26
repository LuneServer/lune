package library

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	videoExts = map[string]bool{
		".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
		".webm": true, ".avi": true, ".ts": true, ".m2ts": true,
	}
	audioExts = map[string]bool{
		".flac": true, ".mp3": true, ".m4a": true, ".opus": true, ".ogg": true, ".wav": true,
	}

	reSeasonEpisode   = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,4})(?:v\d+)?\b`)
	reAltSeasonEp     = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{1,4})(?:v\d+)?\b`)
	reDelimitedYear   = regexp.MustCompile(`[\(\[]((?:19|20)\d{2})[\)\]]`)
	reYear            = regexp.MustCompile(`(?:\(|\[|\b)((?:19|20)\d{2})(?:\)|\]|\b)`)
	reQualityTail     = regexp.MustCompile(`(?i)(?:^|[\s._\-\(\[])(2160p|1080p|720p|480p|4k|uhd|remux|bluray|blu-ray|bdrip|bdremux|web-dl|webrip|hdtv|hdr10|dv|dovi|x265|x264|hevc|av1|h\.?264|h\.?265|hi10p|10bit|8bit|atmos|dts|truehd|flac|aac|dual[\s._-]?audio|multi[\s._-]?sub).*$`)
	reBrackets       = regexp.MustCompile(`\[[^\]]*\]`)
	reCurlyTags      = regexp.MustCompile(`\{[^}]*\}`)
	rePathTMDBID     = regexp.MustCompile(`(?i)[\{\[\(]\s*tmdb(?:id)?[-=:\s]+(\d{1,10})\s*[\}\]\)]`)
	rePathIMDBID     = regexp.MustCompile(`(?i)[\{\[\(]\s*(?:imdb(?:id)?[-=:\s]+)?(tt\d{6,10})\s*[\}\]\)]`)
	reParensMeta     = regexp.MustCompile(`(?i)\([^)]*(?:2160p|1080p|720p|480p|bd|blu-?ray|web-?dl|webrip|hevc|x265|x264|av1|10bit|hi10p|flac|aac|opus|dual[\s._-]?audio|multi[\s._-]?sub|\d{3,4}x\d{3,4}|[0-9a-f]{8})[^)]*\)`)
	reSpaces          = regexp.MustCompile(`\s+`)
	reLeadingGroup    = regexp.MustCompile(`^\s*\[([^\]]{1,40})\]`)
	reAnimeDashEp     = regexp.MustCompile(`(?i)^(.+?)\s+[-–—]\s+(\d{1,4})(?:v\d+)?(?:\s+[-–—]\s+(.+?))?(?:\s*$|\s+[\(\[])`)
	reAnimeEpWord     = regexp.MustCompile(`(?i)^(.+?)\s+(?:episode|ep\.?)\s*(\d{1,4})(?:v\d+)?(?:\s+[-–—:]?\s*(.+?))?$`)
	reLeadingEpNum    = regexp.MustCompile(`(?i)^(\d{1,4})(?:v\d+)?(?:\s*[-–—.]\s*(.+?))?$`)
	reAnimeSpecial    = regexp.MustCompile(`(?i)\b(OVA|OAD|ONA|Special|SP|NCOP|NCED|PV)\s*0*(\d{1,3})?(?:v\d+)?\b`)
	reSeasonFolder    = regexp.MustCompile(`(?i)^(?:season|s|cour|part)\s*0*(\d{1,2})\b`)
	reSpecialsFolder  = regexp.MustCompile(`(?i)^(?:specials?|season\s*0+|ovas?|oads?|extras?|bonus)$`)
	reMoviesFolder    = regexp.MustCompile(`(?i)^(?:movies?|films?|theatrical)$`)
	reOrdinalSeason   = regexp.MustCompile(`(?i)\s+(?:(\d{1,2})(?:st|nd|rd|th)\s+season|season\s+(\d{1,2})|s(\d{1,2}))\s*$`)
	reTechBracketTag  = regexp.MustCompile(`(?i)^(?:2160p|1080p|720p|480p|bd|bdrip|web-?dl|webrip|hevc|x265|x264|av1|10bit|8bit|hi10p|flac|aac|opus|dual[\s._-]?audio|multi[\s._-]?sub|[0-9a-f]{8})$`)
)

// ParsedFile holds metadata inferred from a media file's directory structure and filename.
type ParsedFile struct {
	Kind                  string
	Title                 string
	SortTitle             string
	Year                  *int
	SeriesTitle           string
	SeasonNumber          *int
	EpisodeNumber         *int
	AbsoluteEpisodeNumber *int
	ReleaseGroup          string
	TMDBID                string
	IMDBID                string
	PosterPath            string
	BackdropPath          string
}

// IsSupportedMedia reports whether path is an indexable media file for the given library kind.
func IsSupportedMedia(path, libraryKind string) bool {
	base := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(base, ".") || strings.Contains(base, "sample.") || strings.HasSuffix(base, "-sample"+filepath.Ext(base)) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(base))
	switch libraryKind {
	case "music":
		return audioExts[ext]
	case "movies", "shows", "anime":
		return videoExts[ext]
	default:
		return videoExts[ext] || audioExts[ext]
	}
}

// ParseMediaPath extracts title, year, series/season/episode info, release group, and local sidecar artwork.
func ParseMediaPath(rootPath, fullPath, libraryKind string) ParsedFile {
	base := strings.TrimSuffix(filepath.Base(fullPath), filepath.Ext(fullPath))
	dir := filepath.Dir(fullPath)
	ext := strings.ToLower(filepath.Ext(fullPath))

	var out ParsedFile
	switch {
	case libraryKind == "music" || audioExts[ext]:
		out.Kind = "track"
		out.Title = cleanTitle(base)
		out.SortTitle = makeSortTitle(out.Title)
	case libraryKind == "anime":
		out = parseAnime(rootPath, fullPath, base)
	case libraryKind == "shows" || reSeasonEpisode.MatchString(base):
		out = parseEpisode(rootPath, fullPath, base)
	case libraryKind == "movies":
		out = parseMovie(dir, base)
	default:
		out.Kind = "other"
		out.Title = cleanTitle(base)
		out.SortTitle = makeSortTitle(out.Title)
	}

	out.TMDBID, out.IMDBID = extractPathExternalIDs(rootPath, fullPath, base)
	out.PosterPath, out.BackdropPath = findSidecarArtwork(rootPath, dir)
	return out
}

func parseMovie(dir, base string) ParsedFile {
	out := ParsedFile{Kind: "movie"}
	out.ReleaseGroup = extractReleaseGroup(base)

	cleanedBase := stripAnimeMeta(base)
	if start, _, y, ok := findReleaseYear(cleanedBase); ok {
		out.Year = &y
		out.Title = cleanTitle(cleanedBase[:start])
	}
	if out.Title == "" {
		cleaned := reQualityTail.ReplaceAllString(cleanedBase, "")
		out.Title = cleanTitle(cleaned)
	}
	if out.Year == nil {
		parent := stripExternalIDTags(filepath.Base(dir))
		if _, _, y, ok := findReleaseYear(parent); ok {
			out.Year = &y
		}
	}
	if out.Title == "" {
		out.Title = cleanTitle(base)
	}
	if out.Title == "" {
		out.Title = base
	}
	out.SortTitle = makeSortTitle(out.Title)
	return out
}

func parseEpisode(rootPath, fullPath, base string) ParsedFile {
	out := ParsedFile{Kind: "episode"}
	out.ReleaseGroup = extractReleaseGroup(base)

	if m := reSeasonEpisode.FindStringSubmatchIndex(base); m != nil {
		if s, err := strconv.Atoi(base[m[2]:m[3]]); err == nil {
			out.SeasonNumber = &s
		}
		if e, err := strconv.Atoi(base[m[4]:m[5]]); err == nil {
			out.EpisodeNumber = &e
		}
		prefix := cleanTitle(base[:m[0]])
		suffix := cleanTitle(reQualityTail.ReplaceAllString(stripAnimeMeta(base[m[1]:]), ""))
		if prefix != "" {
			out.SeriesTitle = prefix
		}
		if suffix != "" {
			out.Title = suffix
		} else {
			out.Title = strings.ToUpper(base[m[0]:m[1]])
		}
	} else {
		out.Title = cleanTitle(base)
	}

	// Infer series title from directory hierarchy when missing from filename.
	if out.SeriesTitle == "" {
		inferSeriesFromDirs(rootPath, fullPath, &out)
	}

	sortKey := out.Title
	if out.SeriesTitle != "" {
		sortKey = out.SeriesTitle
	}
	out.SortTitle = makeSortTitle(sortKey)
	return out
}

// parseAnime parses real-world anime filenames and folder layouts:
//   - Fansub releases: [SubsPlease] Sousou no Frieren - 01 (1080p) [A1B2C3D4].mkv
//   - Ordinal/seasoned releases: [Erai-raws] Jujutsu Kaisen 2nd Season - 14 [1080p].mkv
//   - Standard SxxExx (including v2): [EMBER] Attack on Titan (2013) - S04E28 - The Dawn of Humanity.mkv
//   - Absolute numbering: One Piece/One Piece - 1071 [1080p].mkv
//   - OVAs & Specials: [Group] Show - OVA 01.mkv or Specials/NCOP1.mkv
//   - Standalone anime films: Spirited Away (2001)/Spirited Away (2001) [1080p] [Dual Audio].mkv
func parseAnime(rootPath, fullPath, base string) ParsedFile {
	dir := filepath.Dir(fullPath)
	group := extractReleaseGroup(base)

	// Inspect relative directory path for series name, season folder, specials folder, or movies folder.
	folderSeries, folderYear, folderSeason, inSpecialsDir, inMoviesDir := inspectAnimeDirs(rootPath, dir)

	// Normalize underscores to spaces before fansub regex matching (e.g. [HorribleSubs]_Show_-_01_[1080p]).
	work := base
	if group != "" {
		work = reLeadingGroup.ReplaceAllString(work, "")
	}
	work = strings.TrimSpace(work)
	if !strings.Contains(work, " ") && strings.Contains(work, "_") {
		work = strings.ReplaceAll(work, "_", " ")
	}
	// Strip trailing bracketed/parenthesized technical tags while keeping (YYYY).
	cleanWork := stripAnimeMeta(work)

	// Extract embedded year from filename if present.
	var fileYear *int
	if _, _, y, ok := findReleaseYear(cleanWork); ok {
		fileYear = &y
	}

	// 1. Standard SxxExx (e.g. "Attack on Titan (2013) - S04E28 - The Dawn of Humanity")
	if m := reSeasonEpisode.FindStringSubmatchIndex(cleanWork); m != nil {
		out := ParsedFile{Kind: "episode", ReleaseGroup: group}
		if s, err := strconv.Atoi(cleanWork[m[2]:m[3]]); err == nil {
			out.SeasonNumber = &s
		}
		if e, err := strconv.Atoi(cleanWork[m[4]:m[5]]); err == nil {
			out.EpisodeNumber = &e
		}
		prefix := stripYearFromTitle(cleanWork[:m[0]])
		suffix := cleanTitle(reQualityTail.ReplaceAllString(cleanWork[m[1]:], ""))
		out.SeriesTitle = cleanAnimeSeriesTitle(prefix)
		if suffix != "" {
			out.Title = suffix
		} else if out.SeasonNumber != nil && out.EpisodeNumber != nil {
			out.Title = fmt.Sprintf("S%02dE%02d", *out.SeasonNumber, *out.EpisodeNumber)
		}
		out.Year = firstIntPtr(fileYear, folderYear)
		finalizeAnimeEpisode(&out, folderSeries, folderSeason, inSpecialsDir)
		return out
	}

	// 2. Alternate NxNN format (e.g. "Cowboy Bebop 1x05 Ballad of Fallen Angels")
	if m := reAltSeasonEp.FindStringSubmatchIndex(cleanWork); m != nil {
		out := ParsedFile{Kind: "episode", ReleaseGroup: group}
		if s, err := strconv.Atoi(cleanWork[m[2]:m[3]]); err == nil {
			out.SeasonNumber = &s
		}
		if e, err := strconv.Atoi(cleanWork[m[4]:m[5]]); err == nil {
			out.EpisodeNumber = &e
		}
		prefix := stripYearFromTitle(cleanWork[:m[0]])
		suffix := cleanTitle(reQualityTail.ReplaceAllString(cleanWork[m[1]:], ""))
		out.SeriesTitle = cleanAnimeSeriesTitle(prefix)
		if suffix != "" {
			out.Title = suffix
		} else if out.SeasonNumber != nil && out.EpisodeNumber != nil {
			out.Title = fmt.Sprintf("S%02dE%02d", *out.SeasonNumber, *out.EpisodeNumber)
		}
		out.Year = firstIntPtr(fileYear, folderYear)
		finalizeAnimeEpisode(&out, folderSeries, folderSeason, inSpecialsDir)
		return out
	}

	// 3. Explicit OVA / OAD / Special / NCOP / NCED marker -> Season 0
	if !inMoviesDir {
		if m := reAnimeSpecial.FindStringSubmatchIndex(cleanWork); m != nil {
			out := ParsedFile{Kind: "episode", ReleaseGroup: group}
			zero := 0
			out.SeasonNumber = &zero
			if m[4] >= 0 && m[5] > m[4] {
				if e, err := strconv.Atoi(cleanWork[m[4]:m[5]]); err == nil {
					out.EpisodeNumber = &e
				}
			}
			prefix := stripYearFromTitle(cleanWork[:m[0]])
			out.SeriesTitle = cleanAnimeSeriesTitle(prefix)
			specLabel := strings.TrimSpace(cleanWork[m[0]:m[1]])
			tail := cleanTitle(reQualityTail.ReplaceAllString(cleanWork[m[1]:], ""))
			if tail != "" {
				out.Title = specLabel + " - " + tail
			} else {
				out.Title = specLabel
			}
			out.Year = firstIntPtr(fileYear, folderYear)
			finalizeAnimeEpisode(&out, folderSeries, folderSeason, true)
			return out
		}
	}

	// 4. Fansub dash format: "Show Name - 01", "Show Name 2nd Season - 14", "One Piece - 1071", "Show - 01 - Ep Title"
	if !inMoviesDir {
		// Strip trailing quality info after episode number so dash regex anchors cleanly.
		dashWork := strings.TrimSpace(reQualityTail.ReplaceAllString(cleanWork, ""))
		if m := reAnimeDashEp.FindStringSubmatch(dashWork); len(m) >= 3 {
			epNum, err := strconv.Atoi(m[2])
			// Guard against matching a 4-digit year after a dash (e.g. "Title - 2020").
			if err == nil && (epNum < 1900 || epNum > 2099) {
				out := ParsedFile{Kind: "episode", ReleaseGroup: group}
				rawSeries := stripYearFromTitle(m[1])
				seriesName, detectedSeason := extractOrdinalSeason(rawSeries)
				out.SeriesTitle = cleanAnimeSeriesTitle(seriesName)

				if detectedSeason != nil {
					out.SeasonNumber = detectedSeason
				} else if folderSeason != nil {
					out.SeasonNumber = folderSeason
				} else if inSpecialsDir {
					z := 0
					out.SeasonNumber = &z
				} else {
					one := 1
					out.SeasonNumber = &one
				}

				out.EpisodeNumber = &epNum
				// Preserve absolute episode number when no explicit season marker appeared in filename/folder.
				if detectedSeason == nil && folderSeason == nil && !inSpecialsDir {
					abs := epNum
					out.AbsoluteEpisodeNumber = &abs
				}

				epTitle := ""
				if len(m) >= 4 {
					epTitle = cleanTitle(m[3])
				}
				if epTitle != "" {
					out.Title = epTitle
				} else {
					out.Title = fmt.Sprintf("Episode %02d", epNum)
				}
				out.Year = firstIntPtr(fileYear, folderYear)
				finalizeAnimeEpisode(&out, folderSeries, folderSeason, inSpecialsDir)
				return out
			}
		}

		// 5. "Show Name Episode 04" / "Show Name Ep 04"
		if m := reAnimeEpWord.FindStringSubmatch(dashWork); len(m) >= 3 {
			if epNum, err := strconv.Atoi(m[2]); err == nil {
				out := ParsedFile{Kind: "episode", ReleaseGroup: group}
				rawSeries := stripYearFromTitle(m[1])
				seriesName, detectedSeason := extractOrdinalSeason(rawSeries)
				out.SeriesTitle = cleanAnimeSeriesTitle(seriesName)
				if detectedSeason != nil {
					out.SeasonNumber = detectedSeason
				} else if folderSeason != nil {
					out.SeasonNumber = folderSeason
				} else {
					one := 1
					out.SeasonNumber = &one
				}
				out.EpisodeNumber = &epNum
				if detectedSeason == nil && folderSeason == nil && !inSpecialsDir {
					abs := epNum
					out.AbsoluteEpisodeNumber = &abs
				}
				epTitle := ""
				if len(m) >= 4 {
					epTitle = cleanTitle(m[3])
				}
				if epTitle != "" {
					out.Title = epTitle
				} else {
					out.Title = fmt.Sprintf("Episode %02d", epNum)
				}
				out.Year = firstIntPtr(fileYear, folderYear)
				finalizeAnimeEpisode(&out, folderSeries, folderSeason, inSpecialsDir)
				return out
			}
		}

		// 6. Bare episode number inside a series/season directory (e.g. "Frieren/Season 01/01 - The Journey's End.mkv")
		if folderSeries != "" {
			if m := reLeadingEpNum.FindStringSubmatch(dashWork); len(m) >= 2 {
				if epNum, err := strconv.Atoi(m[1]); err == nil && (epNum < 1900 || epNum > 2099) {
					out := ParsedFile{Kind: "episode", ReleaseGroup: group}
					out.SeriesTitle = folderSeries
					if folderSeason != nil {
						out.SeasonNumber = folderSeason
					} else if inSpecialsDir {
						z := 0
						out.SeasonNumber = &z
					} else {
						one := 1
						out.SeasonNumber = &one
					}
					out.EpisodeNumber = &epNum
					if folderSeason == nil && !inSpecialsDir {
						abs := epNum
						out.AbsoluteEpisodeNumber = &abs
					}
					epTitle := ""
					if len(m) >= 3 {
						epTitle = cleanTitle(m[2])
					}
					if epTitle != "" {
						out.Title = epTitle
					} else {
						out.Title = fmt.Sprintf("Episode %02d", epNum)
					}
					out.Year = firstIntPtr(fileYear, folderYear)
					finalizeAnimeEpisode(&out, folderSeries, folderSeason, inSpecialsDir)
					return out
				}
			}
		}
	}

	// 7. Specials folder catch-all (e.g. Specials/Clean Opening.mkv)
	if inSpecialsDir && folderSeries != "" {
		zero := 0
		out := ParsedFile{
			Kind:         "episode",
			SeriesTitle:  folderSeries,
			SeasonNumber: &zero,
			Title:        cleanTitle(reQualityTail.ReplaceAllString(cleanWork, "")),
			Year:         firstIntPtr(fileYear, folderYear),
			ReleaseGroup: group,
		}
		if out.Title == "" {
			out.Title = cleanTitle(base)
		}
		out.SortTitle = makeSortTitle(out.SeriesTitle)
		return out
	}

	// 8. Standalone Anime Movie (e.g. "Spirited Away (2001) [1080p].mkv", "Movies/Your Name (2016).mkv")
	if inMoviesDir || (folderSeason == nil && !inSpecialsDir) {
		mv := parseMovie(dir, base)
		if mv.Year == nil {
			mv.Year = folderYear
		}
		mv.ReleaseGroup = group
		return mv
	}

	// 9. Fallback episode inside a season directory
	out := ParsedFile{
		Kind:         "episode",
		SeriesTitle:  folderSeries,
		SeasonNumber: folderSeason,
		Title:        cleanTitle(reQualityTail.ReplaceAllString(cleanWork, "")),
		Year:         firstIntPtr(fileYear, folderYear),
		ReleaseGroup: group,
	}
	if out.Title == "" {
		out.Title = cleanTitle(base)
	}
	sortKey := out.Title
	if out.SeriesTitle != "" {
		sortKey = out.SeriesTitle
	}
	out.SortTitle = makeSortTitle(sortKey)
	return out
}

func finalizeAnimeEpisode(out *ParsedFile, folderSeries string, folderSeason *int, inSpecialsDir bool) {
	if out.SeriesTitle == "" {
		out.SeriesTitle = folderSeries
	}
	if out.SeasonNumber == nil {
		if inSpecialsDir {
			z := 0
			out.SeasonNumber = &z
		} else if folderSeason != nil {
			out.SeasonNumber = folderSeason
		} else {
			one := 1
			out.SeasonNumber = &one
		}
	}
	if out.Title == "" {
		if out.EpisodeNumber != nil {
			out.Title = fmt.Sprintf("Episode %02d", *out.EpisodeNumber)
		} else {
			out.Title = out.SeriesTitle
		}
	}
	sortKey := out.Title
	if out.SeriesTitle != "" {
		sortKey = out.SeriesTitle
	}
	out.SortTitle = makeSortTitle(sortKey)
}

func inspectAnimeDirs(rootPath, itemDir string) (series string, year *int, season *int, inSpecials, inMovies bool) {
	rel, err := filepath.Rel(rootPath, itemDir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", nil, nil, false, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if reMoviesFolder.MatchString(p) {
			inMovies = true
			continue
		}
		if reSpecialsFolder.MatchString(p) {
			inSpecials = true
			z := 0
			season = &z
			continue
		}
		if sm := reSeasonFolder.FindStringSubmatch(p); len(sm) > 1 {
			if sNum, err := strconv.Atoi(sm[1]); err == nil {
				season = &sNum
				continue
			}
		}
		if i == 0 || series == "" {
			raw := stripExternalIDTags(p)
			if start, _, y, ok := findReleaseYear(raw); ok {
				year = &y
				raw = raw[:start]
			}
			cleaned, ordSeason := extractOrdinalSeason(cleanTitle(raw))
			if ordSeason != nil && season == nil {
				season = ordSeason
			}
			series = cleanAnimeSeriesTitle(cleaned)
		}
	}
	return series, year, season, inSpecials, inMovies
}

func inferSeriesFromDirs(rootPath, fullPath string, out *ParsedFile) {
	rel, err := filepath.Rel(rootPath, filepath.Dir(fullPath))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) > 0 && parts[0] != "" {
		series := stripExternalIDTags(parts[0])
		if start, _, y, ok := findReleaseYear(series); ok {
			out.Year = &y
			series = series[:start]
		}
		out.SeriesTitle = cleanTitle(series)
	}
}

func extractReleaseGroup(base string) string {
	m := reLeadingGroup.FindStringSubmatch(base)
	if len(m) < 2 {
		return ""
	}
	cand := strings.TrimSpace(m[1])
	if cand == "" || reTechBracketTag.MatchString(cand) {
		return ""
	}
	bracketed := "[" + cand + "]"
	if rePathTMDBID.MatchString(bracketed) || rePathIMDBID.MatchString(bracketed) {
		return ""
	}
	return cand
}

// extractPathExternalIDs inspects the filename and parent folders under rootPath for explicit
// provider ID tags such as {tmdb-438631}, [tmdbid-438631], (tmdb-438631), or {imdb-tt1160419}.
func extractPathExternalIDs(rootPath, fullPath, base string) (tmdbID, imdbID string) {
	check := func(s string) {
		if tmdbID == "" {
			if m := rePathTMDBID.FindStringSubmatch(s); len(m) > 1 {
				tmdbID = m[1]
			}
		}
		if imdbID == "" {
			if m := rePathIMDBID.FindStringSubmatch(s); len(m) > 1 {
				imdbID = strings.ToLower(m[1])
			}
		}
	}
	check(base)
	if tmdbID != "" && imdbID != "" {
		return tmdbID, imdbID
	}
	rel, err := filepath.Rel(rootPath, filepath.Dir(fullPath))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return tmdbID, imdbID
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		check(parts[i])
		if tmdbID != "" && imdbID != "" {
			break
		}
	}
	return tmdbID, imdbID
}

func stripExternalIDTags(raw string) string {
	s := rePathTMDBID.ReplaceAllString(raw, " ")
	s = rePathIMDBID.ReplaceAllString(s, " ")
	return s
}

func stripAnimeMeta(raw string) string {
	s := stripExternalIDTags(raw)
	s = reCurlyTags.ReplaceAllString(s, " ")
	s = reBrackets.ReplaceAllString(s, " ")
	s = reParensMeta.ReplaceAllString(s, " ")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// findReleaseYear locates the release year token in a filename or folder name.
// It prefers a parenthesized/bracketed year such as "(2017)" over bare 4-digit numbers
// so titles containing years (e.g. "Blade Runner 2049 (2017)", "2001 A Space Odyssey (1968)")
// keep the year that belongs to the title intact.
func findReleaseYear(s string) (start, end, year int, ok bool) {
	if matches := reDelimitedYear.FindAllStringSubmatchIndex(s, -1); len(matches) > 0 {
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			if cleanTitle(s[:m[0]]) == "" {
				continue
			}
			if y, err := strconv.Atoi(s[m[2]:m[3]]); err == nil {
				return m[0], m[1], y, true
			}
		}
	}
	if matches := reYear.FindAllStringSubmatchIndex(s, -1); len(matches) > 0 {
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			if cleanTitle(s[:m[0]]) == "" {
				continue
			}
			if y, err := strconv.Atoi(s[m[2]:m[3]]); err == nil {
				return m[0], m[1], y, true
			}
		}
	}
	return 0, 0, 0, false
}

func stripYearFromTitle(raw string) string {
	if start, _, _, ok := findReleaseYear(raw); ok {
		// Only strip year if there is a real title before it (avoid stripping "86" or "1984").
		prefix := strings.TrimSpace(raw[:start])
		if prefix != "" {
			return prefix
		}
	}
	return raw
}

func extractOrdinalSeason(raw string) (string, *int) {
	s := cleanTitle(raw)
	if m := reOrdinalSeason.FindStringSubmatchIndex(s); m != nil {
		for _, grp := range [][2]int{{m[2], m[3]}, {m[4], m[5]}, {m[6], m[7]}} {
			if grp[0] >= 0 && grp[1] > grp[0] {
				if n, err := strconv.Atoi(s[grp[0]:grp[1]]); err == nil && n > 0 {
					return strings.TrimSpace(s[:m[0]]), &n
				}
			}
		}
	}
	return s, nil
}

func cleanAnimeSeriesTitle(raw string) string {
	s := cleanTitle(raw)
	s = strings.TrimSpace(strings.Trim(s, "-–—: "))
	return s
}

func firstIntPtr(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}

func cleanTitle(raw string) string {
	s := rePathTMDBID.ReplaceAllString(raw, " ")
	s = rePathIMDBID.ReplaceAllString(s, " ")
	s = reCurlyTags.ReplaceAllString(s, " ")
	s = reBrackets.ReplaceAllString(s, " ")
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	s = strings.TrimSpace(strings.Trim(s, "-–— "))
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func makeSortTitle(title string) string {
	lower := strings.ToLower(strings.TrimSpace(title))
	for _, prefix := range []string{"the ", "an ", "a "} {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(title[len(prefix):])
		}
	}
	return strings.TrimSpace(title)
}

func findSidecarArtwork(rootPath, itemDir string) (poster, backdrop string) {
	posterNames := []string{"poster.jpg", "poster.jpeg", "poster.png", "poster.webp", "folder.jpg", "folder.png", "cover.jpg", "cover.png"}
	backdropNames := []string{"backdrop.jpg", "backdrop.jpeg", "backdrop.png", "backdrop.webp", "fanart.jpg", "fanart.png"}

	// Check item directory first, then parent directory (e.g., Season 01 -> Series folder) up to library root.
	dirs := []string{itemDir}
	parent := filepath.Dir(itemDir)
	if parent != itemDir && isSubpath(rootPath, parent) {
		dirs = append(dirs, parent)
	}

	for _, d := range dirs {
		if poster == "" {
			poster = firstExistingFile(d, posterNames)
		}
		if backdrop == "" {
			backdrop = firstExistingFile(d, backdropNames)
		}
	}
	return poster, backdrop
}

func firstExistingFile(dir string, names []string) string {
	for _, name := range names {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func isSubpath(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
