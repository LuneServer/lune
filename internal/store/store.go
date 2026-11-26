package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps SQLite with split writer/reader pools in WAL mode tuned for
// large self-hosted media libraries (100k+ items).
type Store struct {
	db     *sql.DB // single-connection IMMEDIATE writer
	reader *sql.DB // multi-connection read-only pool
}

// Open initializes the SQLite database in WAL mode with tuned cache/mmap pragmas
// and runs pending migrations.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	// _txlock=immediate acquires write locks up front (avoiding mid-tx SQLITE_BUSY).
	// cache_size=-64000 allocates ~64 MiB page cache; mmap_size=268435456 maps up to 256 MiB.
	writeDSN := fmt.Sprintf(
		"file:%s?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-64000)&_pragma=temp_store(MEMORY)&_pragma=mmap_size(268435456)&_pragma=wal_autocheckpoint(1000)",
		dbPath,
	)
	writer, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	if err := writer.PingContext(ctx); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("ping sqlite writer: %w", err)
	}

	readDSN := fmt.Sprintf(
		"file:%s?_pragma=query_only(ON)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(10000)&_pragma=cache_size(-32000)&_pragma=temp_store(MEMORY)&_pragma=mmap_size(268435456)",
		dbPath,
	)
	reader, err := sql.Open("sqlite", readDSN)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open sqlite reader: %w", err)
	}
	readerConns := runtime.NumCPU()
	if readerConns < 4 {
		readerConns = 4
	}
	if readerConns > 8 {
		readerConns = 8
	}
	reader.SetMaxOpenConns(readerConns)
	reader.SetMaxIdleConns(readerConns)
	reader.SetConnMaxLifetime(0)

	s := &Store{db: writer, reader: reader}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return s, nil
}

// Optimize updates SQLite query-planner statistics for tables whose contents changed significantly.
func (s *Store) Optimize(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// CheckpointPassive runs a non-blocking passive WAL checkpoint.
func (s *Store) CheckpointPassive(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

// Close runs lightweight query-planner optimization + passive WAL checkpoint, then closes both pools.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var errR, errW error
	if s.reader != nil {
		errR = s.reader.Close()
	}
	if s.db != nil {
		_, _ = s.db.Exec(`PRAGMA optimize`)
		_, _ = s.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
		errW = s.db.Close()
	}
	if errW != nil {
		return errW
	}
	return errR
}

// Ping checks database health.
func (s *Store) Ping(ctx context.Context) error {
	return s.reader.PingContext(ctx)
}

// HasUsers reports whether at least one user account exists (used for first-run setup detection).
func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var count int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const createTable = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);`
	if _, err := s.db.ExecContext(ctx, createTable); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE name = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}

		// Toggle foreign_keys OFF outside the transaction so table-rebuilding migrations
		// (e.g. CHECK constraint updates) follow SQLite's safe 12-step ALTER TABLE procedure
		// without triggering ON DELETE CASCADE on child tables.
		if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return fmt.Errorf("migration %s disable fk: %w", name, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			_, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			_, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			_, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
			return err
		}
		if err := tx.Commit(); err != nil {
			_, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
			return err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
			return fmt.Errorf("migration %s enable fk: %w", name, err)
		}
		rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return fmt.Errorf("migration %s fk check: %w", name, err)
		}
		hasViolation := rows.Next()
		_ = rows.Close()
		if hasViolation {
			return fmt.Errorf("migration %s: foreign key integrity check failed", name)
		}
	}
	return nil
}
