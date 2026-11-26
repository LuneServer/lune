package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMediaRequestsStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Create admin and member users; verify default request permissions.
	if err := s.CreateInitialAdmin(ctx, User{
		ID:           "u-admin",
		Username:     "admin",
		DisplayName:  "Admin",
		PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("CreateInitialAdmin: %v", err)
	}
	if err := s.CreateUser(ctx, User{
		ID:           "u-member",
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: "hash",
		Role:         "member",
		AllLibraries: false,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	alice, err := s.GetUserByID(ctx, "u-member")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if !alice.CanRequest || alice.AutoApproveRequests {
		t.Fatalf("unexpected defaults: can_request=%v auto_approve=%v", alice.CanRequest, alice.AutoApproveRequests)
	}

	// Update request permissions via AdminUpdateUser.
	f, tr := false, true
	if err := s.AdminUpdateUser(ctx, "u-member", AdminUserUpdate{
		CanRequest:          &f,
		AutoApproveRequests: &tr,
	}); err != nil {
		t.Fatalf("AdminUpdateUser: %v", err)
	}
	alice, _ = s.GetUserByID(ctx, "u-member")
	if alice.CanRequest || !alice.AutoApproveRequests {
		t.Fatalf("expected updated permissions, got can_request=%v auto_approve=%v", alice.CanRequest, alice.AutoApproveRequests)
	}

	// Create pending movie request.
	y2024 := 2024
	req := MediaRequest{
		ID:         "req-1",
		UserID:     "u-member",
		MediaType:  "movie",
		TMDBID:     693134,
		Title:      "Dune: Part Two",
		Year:       &y2024,
		PosterPath: "/poster.jpg",
		Status:     RequestStatusPending,
	}
	if err := s.CreateMediaRequest(ctx, req); err != nil {
		t.Fatalf("CreateMediaRequest: %v", err)
	}

	// Duplicate active request for same (media_type, tmdb_id) must fail with ErrAlreadyExists.
	dup := req
	dup.ID = "req-dup"
	if err := s.CreateMediaRequest(ctx, dup); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists for duplicate active request, got %v", err)
	}

	// GetMediaRequest joins user info.
	got, err := s.GetMediaRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetMediaRequest: %v", err)
	}
	if got.Username != "alice" || got.DisplayName != "Alice" || got.Title != "Dune: Part Two" {
		t.Fatalf("unexpected GetMediaRequest: %+v", got)
	}

	// CountPendingRequests & FindActiveRequestsByTMDB.
	cnt, err := s.CountPendingRequests(ctx)
	if err != nil || cnt != 1 {
		t.Fatalf("CountPendingRequests = %d, %v; want 1", cnt, err)
	}
	active, err := s.FindActiveRequestsByTMDB(ctx, "movie", []int{693134, 999})
	if err != nil {
		t.Fatalf("FindActiveRequestsByTMDB: %v", err)
	}
	if len(active) != 1 || active[693134].ID != "req-1" {
		t.Fatalf("unexpected active map: %+v", active)
	}

	// Decline the request -> partial unique index now allows a new active request for the same TMDB ID!
	now := time.Now().UTC()
	if err := s.UpdateMediaRequestStatus(ctx, "req-1", MediaRequestStatusUpdate{
		Status:     RequestStatusDeclined,
		StatusNote: "Not now",
		ReviewedBy: "u-admin",
		ReviewedAt: &now,
	}); err != nil {
		t.Fatalf("UpdateMediaRequestStatus: %v", err)
	}
	req2 := req
	req2.ID = "req-2"
	req2.Status = RequestStatusProcessing
	req2.ExternalService = "radarr"
	req2.ExternalID = 42
	if err := s.CreateMediaRequest(ctx, req2); err != nil {
		t.Fatalf("CreateMediaRequest after decline: %v", err)
	}

	// ListMediaRequests filtering.
	all, err := s.ListMediaRequests(ctx, "", "", 50)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListMediaRequests all = %d, %v; want 2", len(all), err)
	}
	onlyProc, err := s.ListMediaRequests(ctx, "u-member", RequestStatusProcessing, 50)
	if err != nil || len(onlyProc) != 1 || onlyProc[0].ID != "req-2" {
		t.Fatalf("ListMediaRequests processing = %+v, %v", onlyProc, err)
	}

	// MarkRequestsAvailableByTMDB transitions processing -> available, leaves declined untouched.
	n, err := s.MarkRequestsAvailableByTMDB(ctx, 693134, "movie")
	if err != nil || n != 1 {
		t.Fatalf("MarkRequestsAvailableByTMDB = %d, %v; want 1", n, err)
	}
	got2, _ := s.GetMediaRequest(ctx, "req-2")
	got1, _ := s.GetMediaRequest(ctx, "req-1")
	if got2.Status != RequestStatusAvailable {
		t.Fatalf("expected req-2 available, got %s", got2.Status)
	}
	if got1.Status != RequestStatusDeclined {
		t.Fatalf("expected req-1 to stay declined, got %s", got1.Status)
	}

	// DeleteMediaRequest.
	if err := s.DeleteMediaRequest(ctx, "req-1"); err != nil {
		t.Fatalf("DeleteMediaRequest: %v", err)
	}
	if _, err := s.GetMediaRequest(ctx, "req-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFindLocalMediaByTMDBWithACLs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.CreateLibrary(ctx, Library{ID: "lib-movies", Name: "Movies", Kind: "movies", RootPath: "/movies"}); err != nil {
		t.Fatalf("CreateLibrary movies: %v", err)
	}
	if err := s.CreateLibrary(ctx, Library{ID: "lib-shows", Name: "Shows", Kind: "shows", RootPath: "/shows"}); err != nil {
		t.Fatalf("CreateLibrary shows: %v", err)
	}

	if err := s.UpsertMediaItem(ctx, MediaItem{
		ID:        "m-1",
		LibraryID: "lib-movies",
		Kind:      "movie",
		Title:     "Inception",
		SortTitle: "inception",
		FilePath:  "/movies/Inception.mkv",
		TMDBID:    "27205",
	}); err != nil {
		t.Fatalf("UpsertMediaItem movie: %v", err)
	}
	if err := s.UpsertMediaItem(ctx, MediaItem{
		ID:          "e-1",
		LibraryID:   "lib-shows",
		Kind:        "episode",
		Title:       "Pilot",
		SortTitle:   "pilot",
		SeriesTitle: "Frieren",
		FilePath:    "/shows/Frieren/S01E01.mkv",
		TMDBID:      "209867",
	}); err != nil {
		t.Fatalf("UpsertMediaItem ep: %v", err)
	}

	// Unrestricted sees both.
	all, err := s.FindLocalMediaByTMDB(ctx, []int{27205, 209867}, nil, false)
	if err != nil {
		t.Fatalf("FindLocalMediaByTMDB: %v", err)
	}
	if len(all) != 2 || all[27205].ItemID != "m-1" || all[209867].SeriesTitle != "Frieren" {
		t.Fatalf("unexpected unrestricted matches: %+v", all)
	}

	// Restricted to lib-movies only sees Inception.
	restricted, err := s.FindLocalMediaByTMDB(ctx, []int{27205, 209867}, []string{"lib-movies"}, true)
	if err != nil {
		t.Fatalf("FindLocalMediaByTMDB restricted: %v", err)
	}
	if len(restricted) != 1 || restricted[27205].ItemID != "m-1" {
		t.Fatalf("unexpected restricted matches: %+v", restricted)
	}

	// Empty allowed list returns empty map.
	none, err := s.FindLocalMediaByTMDB(ctx, []int{27205}, []string{}, true)
	if err != nil || len(none) != 0 {
		t.Fatalf("expected empty matches for empty ACL, got %+v, %v", none, err)
	}
}
