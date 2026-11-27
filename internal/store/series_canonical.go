package store

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// Series grouping keys on media_items.series_title, which is parsed per file. Releases of
// one show often spell it differently ("I'm Looking for Zombie" vs "I'm Looking for a Zombie",
// "Re:Zero" vs "Re Zero"), which splits a show into several series. CanonicalizeSeriesTitles
// rewrites the episodes of one show to a single series_title after each scan:
//
//  1. Episodes that share a series tmdb_id are one show and take the title most of them use
//     (ties go to the title of the earliest episode).
//  2. Episodes without a tmdb_id whose normalised title matches a title variant of exactly one
//     TMDB-bound show join that show.
//  3. The remaining episodes are grouped by normalised title (case, punctuation, and the
//     articles a/an/the ignored) and take the most common spelling.

// SeriesTitleRow is one episode's grouping input.
type SeriesTitleRow struct {
	ID      string
	Title   string
	TMDBID  string
	Season  int
	Episode int
}

// NormalizeSeriesKey folds a series title into a comparison key: lowercase letters and digits
// separated by single spaces, apostrophes dropped, and the articles a/an/the removed.
func NormalizeSeriesKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r == '\'' || r == '’' || r == '`':
			// "I'm" and "Im" compare equal.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	words := strings.Fields(b.String())
	kept := words[:0]
	for _, w := range words {
		if w == "a" || w == "an" || w == "the" {
			continue
		}
		kept = append(kept, w)
	}
	if len(kept) == 0 {
		// A title made only of articles or symbols keeps its plain lowercase form.
		return strings.Join(words, " ")
	}
	return strings.Join(kept, " ")
}

// PlanSeriesTitleMerges returns the episodes whose series_title should change, keyed by item ID.
func PlanSeriesTitleMerges(rows []SeriesTitleRow) map[string]string {
	out := make(map[string]string)
	if len(rows) == 0 {
		return out
	}
	sorted := append([]SeriesTitleRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Season != sorted[j].Season {
			return seasonOrder(sorted[i].Season) < seasonOrder(sorted[j].Season)
		}
		if sorted[i].Episode != sorted[j].Episode {
			return sorted[i].Episode < sorted[j].Episode
		}
		return sorted[i].ID < sorted[j].ID
	})

	assign := func(group []SeriesTitleRow, title string) {
		for _, r := range group {
			if r.Title != title {
				out[r.ID] = title
			}
		}
	}

	// 1. One title per TMDB series.
	byTMDB := make(map[string][]SeriesTitleRow)
	var tmdbOrder []string
	var unbound []SeriesTitleRow
	for _, r := range sorted {
		if r.TMDBID == "" {
			unbound = append(unbound, r)
			continue
		}
		if _, ok := byTMDB[r.TMDBID]; !ok {
			tmdbOrder = append(tmdbOrder, r.TMDBID)
		}
		byTMDB[r.TMDBID] = append(byTMDB[r.TMDBID], r)
	}
	// Normalised title variant -> canonical titles of TMDB shows using it.
	variantOwners := make(map[string]map[string]struct{})
	for _, id := range tmdbOrder {
		group := byTMDB[id]
		canonical := majorityTitle(group)
		assign(group, canonical)
		for _, r := range group {
			key := NormalizeSeriesKey(r.Title)
			if variantOwners[key] == nil {
				variantOwners[key] = make(map[string]struct{})
			}
			variantOwners[key][canonical] = struct{}{}
		}
	}

	// 2. Unbound episodes join the single TMDB show that already uses their title.
	// 3. Otherwise they merge with other unbound episodes by normalised title.
	byKey := make(map[string][]SeriesTitleRow)
	var keyOrder []string
	for _, r := range unbound {
		key := NormalizeSeriesKey(r.Title)
		if owners := variantOwners[key]; len(owners) == 1 {
			for canonical := range owners {
				if r.Title != canonical {
					out[r.ID] = canonical
				}
			}
			continue
		}
		if _, ok := byKey[key]; !ok {
			keyOrder = append(keyOrder, key)
		}
		byKey[key] = append(byKey[key], r)
	}
	for _, key := range keyOrder {
		group := byKey[key]
		assign(group, majorityTitle(group))
	}
	return out
}

// seasonOrder sorts specials (season 0) and unknown seasons after regular seasons so the
// earliest regular episode breaks title ties.
func seasonOrder(season int) int {
	if season <= 0 {
		return 1 << 20
	}
	return season
}

// majorityTitle picks the most common title in group; group is already in episode order, so
// ties go to the title that appears first.
func majorityTitle(group []SeriesTitleRow) string {
	counts := make(map[string]int, 2)
	best, bestCount := "", 0
	for _, r := range group {
		counts[r.Title]++
	}
	for _, r := range group {
		if c := counts[r.Title]; c > bestCount {
			best, bestCount = r.Title, c
		}
	}
	return best
}

// CanonicalizeSeriesTitles merges differently spelled series titles of one show in a library
// (see PlanSeriesTitleMerges) and returns how many episodes were renamed.
func (s *Store) CanonicalizeSeriesTitles(ctx context.Context, libraryID string) (int, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, series_title, tmdb_id, COALESCE(season_number, -1), COALESCE(episode_number, -1)
		FROM media_items
		WHERE library_id = ? AND kind = 'episode' AND series_title != ''`, libraryID)
	if err != nil {
		return 0, err
	}
	var input []SeriesTitleRow
	for rows.Next() {
		var r SeriesTitleRow
		if err := rows.Scan(&r.ID, &r.Title, &r.TMDBID, &r.Season, &r.Episode); err != nil {
			rows.Close()
			return 0, err
		}
		r.TMDBID = strings.TrimSpace(r.TMDBID)
		input = append(input, r)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	plan := PlanSeriesTitleMerges(input)
	if len(plan) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE media_items SET series_title = ? WHERE id = ? AND library_id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for id, title := range plan {
		if _, err := stmt.ExecContext(ctx, title, id, libraryID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(plan), nil
}
