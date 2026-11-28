package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

const (
	chapterThumbTimeout = 30 * time.Second
	chapterThumbBaseVF  = "scale=480:-2:flags=fast_bilinear"
	chapterThumbHDRVF   = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p,scale=480:-2:flags=fast_bilinear"
)

// Busy reasons reported when a task yields to foreground work.
const (
	BusyReasonScan     = "Library scan in progress"
	BusyReasonPlayback = "Playback in progress"
)

// busyReason returns why heavy background work (chapter stills, trickplay, black-bar
// detection) must yield right now, or "" when the server is idle. Playback counts every
// client stream (direct play included), not just FFmpeg HLS sessions.
func (s *Service) busyReason() string {
	return s.busyReasonFor(context.Background())
}

// busyReasonFor is busyReason for the run in ctx: a new-media pass (see newMediaScope) does
// not yield to direct play, only to scans and FFmpeg playback sessions.
func (s *Service) busyReasonFor(ctx context.Context) string {
	if s.isScanRunning != nil && s.isScanRunning() {
		return BusyReasonScan
	}
	if s.activeTranscodeDirs != nil && len(s.activeTranscodeDirs()) > 0 {
		return BusyReasonPlayback
	}
	if newMediaScopeFrom(ctx) != nil {
		return ""
	}
	if s.isPlaybackActive != nil && s.isPlaybackActive() {
		return BusyReasonPlayback
	}
	return ""
}

func (s *Service) isBusyForChapterThumbnails(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	reason := s.busyReasonFor(ctx)
	if reason == "" {
		return false
	}
	s.mu.Lock()
	s.lastBusyReason = reason
	s.mu.Unlock()
	return true
}

// chapterThumbnailSeekMS computes the seek timestamp (in ms) for a chapter still:
// startMS + min(5000, chapterDurationMS/4), or startMS + 3000 when EndMS <= StartMS.
func chapterThumbnailSeekMS(ch metadata.ChapterInfo) int64 {
	startMS := ch.StartMS
	if startMS < 0 {
		startMS = 0
	}
	if ch.EndMS <= startMS {
		return startMS + 3000
	}
	offset := (ch.EndMS - startMS) / 4
	if offset > 5000 {
		offset = 5000
	}
	return startMS + offset
}

type chapterThumbResult struct {
	Generated     int
	Existing      int
	Total         int
	MissingFiles  int
	Unextractable int
	SkippedBusy   bool
}

// chapterFailedMarkerExt names the empty marker file recorded beside a chapter still that
// FFmpeg could not extract, so the same file version is not retried on every run.
const chapterFailedMarkerExt = ".failed"

// chapterStillUpToDate reports whether path exists and is at least as new as the media file.
// fileMtimeNS is media_items.file_mtime, which the scanner stores as Unix nanoseconds.
func chapterStillUpToDate(path string, fileMtimeNS int64) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.ModTime().UnixNano() >= fileMtimeNS
}

func markChapterStillFailed(path string) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	_ = f.Close()
}

type pendingChapterWork struct {
	item         store.ChapterSourceItem
	itemDir      string
	pending      []metadata.ChapterInfo
	validIndices map[int]struct{}
	meta         chapterStillsMeta
	current      bool  // meta matches this file version and stills version
	durationMS   int64 // approximate runtime (last chapter end) for bar detection
}

// chapterStillsVersion bumps whenever stills must be rebuilt for every title
// (v2: black bars cropped).
const chapterStillsVersion = 2

// chapterStillsMetaName records, per title, how its stills were built.
const chapterStillsMetaName = "stills.json"

type chapterStillsMeta struct {
	Version     int    `json:"version"`
	SourceMtime int64  `json:"source_mtime"` // media_items.file_mtime (ns)
	Crop        string `json:"crop,omitempty"`
}

var errChapterBusy = errors.New("server busy")

func readChapterStillsMeta(itemDir string) chapterStillsMeta {
	var m chapterStillsMeta
	if raw, err := os.ReadFile(filepath.Join(itemDir, chapterStillsMetaName)); err == nil {
		_ = json.Unmarshal(raw, &m)
	}
	return m
}

// removeChapterStills deletes an item's stills and failure markers (not in-flight temp files).
func removeChapterStills(itemDir string) {
	entries, _ := os.ReadDir(itemDir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.Contains(name, ".tmp") {
			continue
		}
		if strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, chapterFailedMarkerExt) {
			_ = os.Remove(filepath.Join(itemDir, name))
		}
	}
}

func (s *Service) generateChapterThumbnails(ctx context.Context, onProgress func(done, total int, detail string)) (chapterThumbResult, error) {
	var res chapterThumbResult
	if s.ffmpegBin == "" {
		return res, nil
	}
	if s.isBusyForChapterThumbnails(ctx) {
		res.SkippedBusy = true
		return res, nil
	}

	items, err := s.store.ListItemsWithChapters(ctx)
	if err != nil {
		return res, err
	}

	chaptersRoot := filepath.Join(s.dataDir, "artwork", "chapters")
	var work []pendingChapterWork
	totalPendingStills := 0

	scope := newMediaScopeFrom(ctx)
	for _, item := range items {
		if !scope.includes(item.ID) {
			continue
		}
		var chapters []metadata.ChapterInfo
		if err := json.Unmarshal([]byte(item.ChaptersJSON), &chapters); err != nil || len(chapters) == 0 {
			continue
		}

		itemDir := filepath.Join(chaptersRoot, item.ID)
		meta := readChapterStillsMeta(itemDir)
		current := meta.Version == chapterStillsVersion && meta.SourceMtime == item.FileMtime
		validIndices := make(map[int]struct{}, len(chapters))
		var pending []metadata.ChapterInfo
		var durationMS int64
		for _, ch := range chapters {
			durationMS = max(durationMS, ch.EndMS, ch.StartMS)
			if ch.Index < 0 {
				continue
			}
			res.Total++
			validIndices[ch.Index] = struct{}{}
			if !current {
				// Built by an older stills version (e.g. with black bars) or for an older file.
				pending = append(pending, ch)
				continue
			}
			if chapterStillUpToDate(filepath.Join(itemDir, strconv.Itoa(ch.Index)+".jpg"), item.FileMtime) {
				res.Existing++
				continue
			}
			if chapterStillUpToDate(filepath.Join(itemDir, strconv.Itoa(ch.Index)+chapterFailedMarkerExt), item.FileMtime) {
				// A previous attempt failed for this exact file version (e.g. the chapter starts
				// past the last decodable frame); don't burn FFmpeg time on it every run.
				res.Unextractable++
				continue
			}
			pending = append(pending, ch)
		}

		pruneStaleChapterStills(itemDir, validIndices)

		if len(pending) == 0 {
			continue
		}
		if _, err := os.Stat(item.FilePath); err != nil {
			res.MissingFiles++
			continue
		}
		work = append(work, pendingChapterWork{
			item:         item,
			itemDir:      itemDir,
			pending:      pending,
			validIndices: validIndices,
			meta:         meta,
			current:      current,
			durationMS:   durationMS,
		})
		totalPendingStills += len(pending)
	}

	if totalPendingStills == 0 {
		return res, nil
	}

	attempted := 0
	for _, w := range work {
		if s.isBusyForChapterThumbnails(ctx) {
			res.SkippedBusy = true
			return res, nil
		}
		if err := os.MkdirAll(w.itemDir, 0o755); err != nil {
			s.logger.Warn("failed to create chapter thumbnail directory", "item_id", w.item.ID, "error", err)
			attempted += len(w.pending)
			continue
		}

		label := strings.TrimSpace(w.item.Title)
		if label == "" {
			label = filepath.Base(w.item.FilePath)
		}
		isHDR := w.item.VideoHDR != "" && !strings.EqualFold(w.item.VideoHDR, "sdr")

		if !w.current {
			// Detect letterbox/pillarbox bars once per file version, then rebuild all stills.
			crop, err := s.detectBlackBars(ctx, w.item.FilePath, w.durationMS)
			if errors.Is(err, errChapterBusy) || ctx.Err() != nil {
				res.SkippedBusy = ctx.Err() == nil
				return res, nil
			}
			if err != nil {
				s.logger.Debug("black bar detection failed; keeping full frame", "item_id", w.item.ID, "error", err)
			}
			removeChapterStills(w.itemDir)
			w.meta = chapterStillsMeta{Version: chapterStillsVersion, SourceMtime: w.item.FileMtime, Crop: crop}
			if b, err := json.Marshal(w.meta); err == nil {
				_ = os.WriteFile(filepath.Join(w.itemDir, chapterStillsMetaName), b, 0o644)
			}
		}

		for _, ch := range w.pending {
			if s.isBusyForChapterThumbnails(ctx) {
				res.SkippedBusy = true
				return res, nil
			}
			if onProgress != nil {
				onProgress(attempted, totalPendingStills, fmt.Sprintf("%s (%d/%d)", label, attempted+1, totalPendingStills))
			}

			targetPath := filepath.Join(w.itemDir, strconv.Itoa(ch.Index)+".jpg")
			tmpPath := filepath.Join(w.itemDir, strconv.Itoa(ch.Index)+".tmp.jpg")
			seekSec := fmt.Sprintf("%.3f", float64(chapterThumbnailSeekMS(ch))/1000.0)

			if err := s.extractChapterStill(ctx, w.item.FilePath, seekSec, isHDR, w.meta.Crop, tmpPath); err != nil {
				_ = os.Remove(tmpPath)
				if ctx.Err() != nil {
					// Canceled mid-extraction: not a property of the file, retry next time.
					return res, nil
				}
				s.logger.Debug("chapter thumbnail extraction failed", "item_id", w.item.ID, "chapter", ch.Index, "error", err)
				markChapterStillFailed(filepath.Join(w.itemDir, strconv.Itoa(ch.Index)+chapterFailedMarkerExt))
				res.Unextractable++
				attempted++
				continue
			}
			if err := os.Rename(tmpPath, targetPath); err != nil {
				_ = os.Remove(tmpPath)
				s.logger.Warn("failed to commit chapter thumbnail", "item_id", w.item.ID, "chapter", ch.Index, "error", err)
				attempted++
				continue
			}
			res.Generated++
			res.Existing++
			attempted++
			if onProgress != nil {
				onProgress(attempted, totalPendingStills, fmt.Sprintf("%s (%d/%d)", label, attempted, totalPendingStills))
			}
		}
	}

	return res, nil
}

func (s *Service) extractChapterStill(ctx context.Context, filePath, seekSec string, isHDR bool, crop, tmpPath string) error {
	runFFmpeg := func(vf string) error {
		if crop != "" {
			vf = crop + "," + vf
		}
		cmdCtx, cancel := context.WithTimeout(ctx, chapterThumbTimeout)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, s.ffmpegBin,
			"-nostdin",
			"-loglevel", "error",
			// -threads before -i only limits the decoder; filters and the encoder need
			// their own limits or they start a thread per core.
			"-threads", "1",
			"-filter_threads", "1",
			"-ss", seekSec,
			"-i", filePath,
			"-map", "0:v:0",
			"-frames:v", "1",
			"-vf", vf,
			"-threads", "1",
			"-q:v", "4",
			"-f", "image2",
			"-y",
			tmpPath,
		)
		if err := runBackground(cmd); err != nil {
			return err
		}
		info, err := os.Stat(tmpPath)
		if err != nil || info.IsDir() || info.Size() == 0 {
			return fmt.Errorf("ffmpeg produced empty output")
		}
		return nil
	}

	if isHDR {
		if err := runFFmpeg(chapterThumbHDRVF); err == nil {
			return nil
		}
		_ = os.Remove(tmpPath)
	}
	return runFFmpeg(chapterThumbBaseVF)
}

func pruneStaleChapterStills(itemDir string, validIndices map[int]struct{}) {
	entries, err := os.ReadDir(itemDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".tmp.jpg") {
			continue
		}
		var stem string
		switch {
		case strings.HasSuffix(name, ".jpg"):
			stem = strings.TrimSuffix(name, ".jpg")
		case strings.HasSuffix(name, chapterFailedMarkerExt):
			stem = strings.TrimSuffix(name, chapterFailedMarkerExt)
		default:
			continue
		}
		idx, err := strconv.Atoi(stem)
		if err != nil || strconv.Itoa(idx) != stem {
			continue
		}
		if _, ok := validIndices[idx]; !ok {
			_ = os.Remove(filepath.Join(itemDir, name))
		}
	}
}
