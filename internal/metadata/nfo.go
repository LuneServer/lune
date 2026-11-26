package metadata

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CreditPerson represents a cast or crew member associated with a media item.
type CreditPerson struct {
	Name        string `json:"name"`
	Role        string `json:"role,omitempty"`
	Department  string `json:"department,omitempty"` // "cast" | "crew"
	ProfilePath string `json:"profile_path,omitempty"`
}

// EnrichedInfo holds descriptive metadata sourced from local NFO files or remote providers.
type EnrichedInfo struct {
	Title         string
	SeriesTitle   string
	OriginalTitle string
	Overview      string
	Tagline       string
	Year          *int
	Rating        float64
	RuntimeMS     int64
	ContentRating string
	ReleaseDate   string
	Genres        []string
	Directors     []string
	Writers       []string
	Studios       []string
	Cast          []CreditPerson
	TMDBID        string
	IMDBID        string
	PosterPath    string
	BackdropPath  string
	MappedSeason  *int
	MappedEpisode *int
}

type nfoXML struct {
	XMLName       xml.Name `xml:""`
	Title         string   `xml:"title"`
	OriginalTitle string   `xml:"originaltitle"`
	Plot          string   `xml:"plot"`
	Outline       string   `xml:"outline"`
	Tagline       string   `xml:"tagline"`
	Year          string   `xml:"year"`
	Rating        string   `xml:"rating"`
	MPAA          string   `xml:"mpaa"`
	Premiered     string   `xml:"premiered"`
	Aired         string   `xml:"aired"`
	Genres        []string `xml:"genre"`
	Directors     []string `xml:"director"`
	Credits       []string `xml:"credits"`
	Studios       []string `xml:"studio"`
	Actors        []struct {
		Name string `xml:"name"`
		Role string `xml:"role"`
	} `xml:"actor"`
	TMDBID    string `xml:"tmdbid"`
	IMDBID    string `xml:"imdbid"`
	UniqueIDs []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
}

// ParseLocalNFO checks for a sidecar .nfo file matching mediaPath (or movie.nfo / tvshow.nfo)
// and extracts standard Kodi/Radarr/Sonarr XML metadata offline.
func ParseLocalNFO(mediaPath string) (EnrichedInfo, bool) {
	return ParseLocalNFOForKind("", mediaPath, "")
}

// ParseLocalNFOForKind extracts local Kodi/Radarr/Sonarr XML metadata for mediaPath,
// restricting series-level tvshow.nfo inheritance to episodes (or unspecified kind)
// and never traversing above rootPath when rootPath is non-empty.
func ParseLocalNFOForKind(rootPath, mediaPath, kind string) (EnrichedInfo, bool) {
	stem := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath))
	dir := filepath.Dir(mediaPath)
	parentDir := filepath.Dir(dir)

	// 1. Prefer item-specific sidecar (<stem>.nfo, plus movie.nfo for movies/unspecified).
	itemCandidates := []string{stem + ".nfo"}
	if kind != "episode" {
		itemCandidates = append(itemCandidates, filepath.Join(dir, "movie.nfo"))
	}
	var itemInfo EnrichedInfo
	foundItem := false
	for _, p := range itemCandidates {
		if info, ok := parseNFOFile(p); ok {
			itemInfo = info
			foundItem = true
			break
		}
	}

	// 2. For movies, never inherit series-level tvshow.nfo from the current or parent directory.
	if kind == "movie" {
		if !foundItem {
			return EnrichedInfo{}, false
		}
		return itemInfo, true
	}

	// 3. For episodes (or unspecified kind), check series-level tvshow.nfo in dir or parentDir
	// while staying strictly inside rootPath when configured.
	tvCandidates := []string{filepath.Join(dir, "tvshow.nfo")}
	if parentDir != dir && (rootPath == "" || isWithinDir(rootPath, parentDir)) {
		tvCandidates = append(tvCandidates, filepath.Join(parentDir, "tvshow.nfo"))
	}
	var tvInfo EnrichedInfo
	foundTV := false
	for _, p := range tvCandidates {
		if info, ok := parseNFOFile(p); ok {
			tvInfo = info
			foundTV = true
			break
		}
	}

	if !foundItem && !foundTV {
		return EnrichedInfo{}, false
	}
	if !foundItem {
		// Return only series identity and explicitly shared attributes so episode-specific
		// fields (plot, rating, air date, credits) remain empty for remote episode enrichment.
		return EnrichedInfo{
			SeriesTitle:   tvInfo.SeriesTitle,
			TMDBID:        tvInfo.TMDBID,
			IMDBID:        tvInfo.IMDBID,
			ContentRating: tvInfo.ContentRating,
			Genres:        tvInfo.Genres,
			Studios:       tvInfo.Studios,
		}, true
	}
	if foundTV {
		// Inherit series-level identity and shared metadata missing from the episode's own .nfo.
		if itemInfo.SeriesTitle == "" {
			itemInfo.SeriesTitle = tvInfo.SeriesTitle
		}
		if itemInfo.TMDBID == "" {
			itemInfo.TMDBID = tvInfo.TMDBID
		}
		if itemInfo.IMDBID == "" {
			itemInfo.IMDBID = tvInfo.IMDBID
		}
		if itemInfo.ContentRating == "" {
			itemInfo.ContentRating = tvInfo.ContentRating
		}
		if len(itemInfo.Genres) == 0 {
			itemInfo.Genres = tvInfo.Genres
		}
		if len(itemInfo.Studios) == 0 {
			itemInfo.Studios = tvInfo.Studios
		}
	}
	return itemInfo, true
}

func isWithinDir(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func parseNFOFile(path string) (EnrichedInfo, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return EnrichedInfo{}, false
	}

	var doc nfoXML
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return EnrichedInfo{}, false
	}

	isEpisodeDoc := strings.EqualFold(doc.XMLName.Local, "episodedetails")
	info := EnrichedInfo{
		Title:         strings.TrimSpace(doc.Title),
		OriginalTitle: strings.TrimSpace(doc.OriginalTitle),
		Overview:      strings.TrimSpace(doc.Plot),
		Tagline:       strings.TrimSpace(doc.Tagline),
		ContentRating: strings.TrimSpace(doc.MPAA),
		ReleaseDate:   strings.TrimSpace(doc.Premiered),
	}
	// Episode-level <episodedetails> IDs identify the individual episode rather than the TV series,
	// so keep them out of TMDBID/IMDBID (which represent series identity for episodes).
	if !isEpisodeDoc {
		info.TMDBID = strings.TrimSpace(doc.TMDBID)
		info.IMDBID = strings.TrimSpace(doc.IMDBID)
	}
	if strings.EqualFold(doc.XMLName.Local, "tvshow") {
		info.SeriesTitle = info.Title
		info.Title = ""
	}
	if info.ReleaseDate == "" {
		info.ReleaseDate = strings.TrimSpace(doc.Aired)
	}
	if info.Overview == "" {
		info.Overview = strings.TrimSpace(doc.Outline)
	}
	if y, err := strconv.Atoi(strings.TrimSpace(doc.Year)); err == nil && y > 1880 && y < 2200 {
		info.Year = &y
	}
	if r, err := strconv.ParseFloat(strings.TrimSpace(doc.Rating), 64); err == nil && r > 0 && r <= 10 {
		info.Rating = r
	}
	for _, g := range doc.Genres {
		if g = strings.TrimSpace(g); g != "" {
			info.Genres = append(info.Genres, g)
		}
	}
	for _, d := range doc.Directors {
		if d = strings.TrimSpace(d); d != "" {
			info.Directors = append(info.Directors, d)
		}
	}
	for _, w := range doc.Credits {
		if w = strings.TrimSpace(w); w != "" {
			info.Writers = append(info.Writers, w)
		}
	}
	for _, st := range doc.Studios {
		if st = strings.TrimSpace(st); st != "" {
			info.Studios = append(info.Studios, st)
		}
	}
	for _, a := range doc.Actors {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		info.Cast = append(info.Cast, CreditPerson{
			Name:       name,
			Role:       strings.TrimSpace(a.Role),
			Department: "cast",
		})
		if len(info.Cast) >= 18 {
			break
		}
	}
	if !isEpisodeDoc {
		for _, uid := range doc.UniqueIDs {
			val := strings.TrimSpace(uid.Value)
			switch strings.ToLower(uid.Type) {
			case "tmdb":
				if info.TMDBID == "" {
					info.TMDBID = val
				}
			case "imdb":
				if info.IMDBID == "" {
					info.IMDBID = val
				}
			}
		}
	}
	return info, true
}
