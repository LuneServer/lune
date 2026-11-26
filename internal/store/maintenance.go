package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// PruneAuditEventsBefore removes audit_events older than cutoff and returns the number of deleted rows.
func (s *Store) PruneAuditEventsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM audit_events WHERE created_at < ?`, formatTime(cutoff))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListReferencedArtworkPaths returns the set of cleaned poster/backdrop file paths and
// person portrait basenames currently referenced by indexed media items.
func (s *Store) ListReferencedArtworkPaths(ctx context.Context) (files map[string]struct{}, personFiles map[string]struct{}, err error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT poster_path, backdrop_path, cast_json FROM media_items`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	files = make(map[string]struct{})
	personFiles = make(map[string]struct{})

	for rows.Next() {
		var poster, backdrop, castJSON string
		if err := rows.Scan(&poster, &backdrop, &castJSON); err != nil {
			return nil, nil, err
		}
		if p := strings.TrimSpace(poster); p != "" {
			files[filepath.Clean(p)] = struct{}{}
		}
		if b := strings.TrimSpace(backdrop); b != "" {
			files[filepath.Clean(b)] = struct{}{}
		}
		if castJSON != "" && castJSON != "[]" {
			var cast []struct {
				ProfilePath string `json:"profile_path"`
			}
			if json.Unmarshal([]byte(castJSON), &cast) == nil {
				for _, c := range cast {
					pp := strings.TrimPrefix(strings.TrimSpace(c.ProfilePath), "/")
					if pp != "" && !strings.Contains(pp, "/") && !strings.Contains(pp, "\\") {
						personFiles[pp] = struct{}{}
					}
				}
			}
		}
	}
	return files, personFiles, rows.Err()
}
