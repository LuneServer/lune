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
	stem := strings.TrimSuffix(mediaPath, filepath.Ext(mediaPath))
	dir := filepath.Dir(mediaPath)
	candidates := []string{
		stem + ".nfo",
		filepath.Join(dir, "movie.nfo"),
		filepath.Join(dir, "tvshow.nfo"),
	}

	var raw []byte
	var err error
	for _, p := range candidates {
		raw, err = os.ReadFile(p)
		if err == nil && len(raw) > 0 {
			break
		}
	}
	if len(raw) == 0 {
		return EnrichedInfo{}, false
	}

	var doc nfoXML
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return EnrichedInfo{}, false
	}

	info := EnrichedInfo{
		Title:         strings.TrimSpace(doc.Title),
		OriginalTitle: strings.TrimSpace(doc.OriginalTitle),
		Overview:      strings.TrimSpace(doc.Plot),
		Tagline:       strings.TrimSpace(doc.Tagline),
		ContentRating: strings.TrimSpace(doc.MPAA),
		ReleaseDate:   strings.TrimSpace(doc.Premiered),
		TMDBID:        strings.TrimSpace(doc.TMDBID),
		IMDBID:        strings.TrimSpace(doc.IMDBID),
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
	return info, true
}
