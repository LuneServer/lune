package library

import (
	"context"
	"errors"

	"github.com/LuneServer/lune/internal/store"
)

func (s *Service) ListSeries(ctx context.Context, libraryID, query, sortBy string, allowedLibraryIDs []string, restrictLibraries bool) ([]store.SeriesSummary, error) {
	return s.store.ListSeries(ctx, libraryID, query, sortBy, allowedLibraryIDs, restrictLibraries)
}

func (s *Service) ListSeriesFiltered(ctx context.Context, f store.SeriesListFilter) ([]store.SeriesSummary, error) {
	return s.store.ListSeriesFiltered(ctx, f)
}

func (s *Service) ListSeriesEpisodes(ctx context.Context, libraryID, seriesTitle, userID string, allowedLibraryIDs []string, restrictLibraries bool) ([]store.EpisodeWithProgress, error) {
	eps, err := s.store.ListSeriesEpisodes(ctx, libraryID, seriesTitle, userID, allowedLibraryIDs, restrictLibraries)
	if err != nil {
		return nil, err
	}
	if len(eps) == 0 {
		return nil, ErrNotFound
	}
	return eps, nil
}

func (s *Service) GetNextEpisode(ctx context.Context, itemID string) (store.MediaItem, error) {
	next, err := s.store.GetNextEpisode(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return store.MediaItem{}, ErrNotFound
	}
	return next, err
}

func (s *Service) FindSimilarMedia(ctx context.Context, seed store.MediaItem, allowedLibraryIDs []string, restrictLibraries bool, limit int) ([]store.MediaItem, []store.SeriesSummary, error) {
	return s.store.FindSimilarMedia(ctx, seed, allowedLibraryIDs, restrictLibraries, limit)
}

func (s *Service) SetSeriesWatched(ctx context.Context, userID, libraryID, seriesTitle string, seasonNumber *int, completed bool) (int, error) {
	n, err := s.store.SetSeriesWatched(ctx, userID, libraryID, seriesTitle, seasonNumber, completed)
	if errors.Is(err, store.ErrNotFound) {
		return 0, ErrNotFound
	}
	return n, err
}

// RefreshSeries re-probes and re-enriches every episode of a series, optionally re-matching
// it under a new series title, year, and/or explicit TMDB TV ID.
// Returns the number of updated episodes and the resolved canonical series title.
func (s *Service) RefreshSeries(ctx context.Context, libraryID, seriesTitle, overrideTitle string, overrideYear *int, overrideTMDBID ...string) (int, string, error) {
	eps, err := s.store.ListSeriesEpisodes(ctx, libraryID, seriesTitle, "", nil, false)
	if err != nil {
		return 0, "", err
	}
	if len(eps) == 0 {
		return 0, "", ErrNotFound
	}
	updated := 0
	resolvedTitle := seriesTitle
	for _, ep := range eps {
		refreshed, err := s.RefreshItem(ctx, ep.Item.ID, overrideTitle, overrideYear, overrideTMDBID...)
		if err != nil {
			continue
		}
		if refreshed.SeriesTitle != "" {
			resolvedTitle = refreshed.SeriesTitle
		}
		updated++
	}
	return updated, resolvedTitle, nil
}
