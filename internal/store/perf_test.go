package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteOptimizations_BatchFTS5AndCoveringIndex(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lune.db")

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.CreateLibrary(ctx, Library{
		ID:       "lib-movies",
		Name:     "Movies",
		Kind:     "movies",
		RootPath: "/media/movies",
	}); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	// Batch upsert 2,000 items to exercise high-throughput prepared transactions + FTS5 triggers.
	items := make([]MediaItem, 0, 2002)
	for i := 0; i < 2000; i++ {
		items = append(items, MediaItem{
			ID:        fmt.Sprintf("item-%04d", i),
			LibraryID: "lib-movies",
			Kind:      "movie",
			Title:     fmt.Sprintf("Catalog Movie %04d", i),
			SortTitle: fmt.Sprintf("Catalog Movie %04d", i),
			FilePath:  fmt.Sprintf("/media/movies/movie-%04d.mkv", i),
			FileSize:  1_000_000 + int64(i),
			FileMtime: 1700000000 + int64(i),
		})
	}
	items = append(items,
		MediaItem{
			ID:        "item-interstellar",
			LibraryID: "lib-movies",
			Kind:      "movie",
			Title:     "Interstellar",
			SortTitle: "Interstellar",
			Overview:  "Explorers travel through a wormhole in space.",
			FilePath:  "/media/movies/Interstellar.mkv",
		},
		MediaItem{
			ID:          "item-shogun",
			LibraryID:   "lib-movies",
			Kind:        "episode",
			Title:       "Anjin",
			SortTitle:   "Shōgun",
			SeriesTitle: "Shōgun",
			FilePath:    "/media/shows/Shogun.S01E01.mkv",
		},
	)

	if err := s.UpsertMediaItemsBatch(ctx, items); err != nil {
		t.Fatalf("UpsertMediaItemsBatch: %v", err)
	}

	// 1. FTS5 prefix search matches title.
	res, total, err := s.ListMediaItems(ctx, MediaListFilter{Query: "inter"})
	if err != nil || total != 1 || res[0].ID != "item-interstellar" {
		t.Fatalf("prefix FTS search failed: total=%d err=%v", total, err)
	}

	// 2. FTS5 matches overview text as well.
	res, total, err = s.ListMediaItems(ctx, MediaListFilter{Query: "wormhole"})
	if err != nil || total != 1 || res[0].ID != "item-interstellar" {
		t.Fatalf("overview FTS search failed: total=%d err=%v", total, err)
	}

	// 3. FTS5 diacritic-insensitive series search (`shogun` -> `Shōgun`).
	series, err := s.ListSeries(ctx, "", "shogun", "title", nil, false)
	if err != nil || len(series) != 1 || series[0].Title != "Shōgun" {
		t.Fatalf("diacritic series FTS search failed: series=%+v err=%v", series, err)
	}

	// 4. Arbitrary punctuation / FTS5 operators never cause a query error.
	if _, _, err := s.ListMediaItems(ctx, MediaListFilter{Query: `"inter"* OR (NOT) -- : ^`}); err != nil {
		t.Fatalf("FTS sanitization failed: %v", err)
	}

	// 5. Verify ListLibraryFingerprints uses the covering index (no main table lookup).
	var planDetails []string
	rows, err := s.reader.QueryContext(ctx, `
		EXPLAIN QUERY PLAN
		SELECT id, file_path, file_size, file_mtime, metadata_version FROM media_items WHERE library_id = ?
	`, "lib-movies")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err == nil {
			planDetails = append(planDetails, detail)
		}
	}
	joinedPlan := strings.Join(planDetails, " | ")
	if !strings.Contains(joinedPlan, "COVERING INDEX idx_media_items_fingerprints") {
		t.Fatalf("expected covering index idx_media_items_fingerprints, got plan: %s", joinedPlan)
	}
}
