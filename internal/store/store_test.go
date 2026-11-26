package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lune.db")

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	hasUsers, err := s.HasUsers(ctx)
	if err != nil {
		t.Fatalf("HasUsers failed: %v", err)
	}
	if hasUsers {
		t.Fatal("expected fresh database to have no users")
	}

	// Re-opening should be idempotent across migrations.
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	s2, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer s2.Close()
}
