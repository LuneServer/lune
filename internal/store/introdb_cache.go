package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GetIntroDBCache returns a cached IntroDB lookup payload and when it was fetched.
// It returns ErrNotFound when the key has never been cached.
func (s *Store) GetIntroDBCache(ctx context.Context, key string) (string, time.Time, error) {
	var payload string
	var fetched int64
	err := s.reader.QueryRowContext(ctx,
		`SELECT payload, fetched_at FROM introdb_cache WHERE lookup_key = ?`, key,
	).Scan(&payload, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return payload, time.Unix(fetched, 0), nil
}

// PutIntroDBCache upserts a cached IntroDB lookup payload.
func (s *Store) PutIntroDBCache(ctx context.Context, key, payload string, fetchedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO introdb_cache (lookup_key, payload, fetched_at)
		VALUES (?, ?, ?)
		ON CONFLICT(lookup_key) DO UPDATE SET
			payload    = excluded.payload,
			fetched_at = excluded.fetched_at
	`, key, payload, fetchedAt.Unix())
	return err
}

// PruneIntroDBCache deletes cached lookups fetched before cutoff and returns how many were removed.
func (s *Store) PruneIntroDBCache(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM introdb_cache WHERE fetched_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
