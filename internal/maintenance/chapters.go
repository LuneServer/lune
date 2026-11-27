package maintenance

import (
	"context"
	"encoding/json"
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
	maxChapterThumbItemsPerRun = 200
	chapterThumbTimeout        = 30 * time.Second
	chapterThumbBaseVF         = "scale=480:-2:flags=fast_bilinear"
	chapterThumbHDRVF          = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p,scale=480:-2:flags=fast_bilinear"
)

func (s *Service) isBusyForChapterThumbnails(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	if s.isScanRunning != nil && s.isScanRunning() {
		return true
	}
	if s.activeTranscodeDirs != nil && len(s.activeTranscodeDirs()) > 0 {
		return true
	}
	return false
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
	Generated    int
	Existing     int
	Total        int
	MissingFiles int
	SkippedBusy  bool
}

type pendingChapterWork struct {
	item         store.ChapterSourceItem
	itemDir      string
	pending      []metadata.ChapterInfo
	validIndices map[int]struct{}
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

	for _, item := range items {
		var chapters []metadata.ChapterInfo
		if err := json.Unmarshal([]byte(item.ChaptersJSON), &chapters); err != nil || len(chapters) == 0 {
			continue
		}

		itemDir := filepath.Join(chaptersRoot, item.ID)
		validIndices := make(map[int]struct{}, len(chapters))
		var pending []metadata.ChapterInfo
		for _, ch := range chapters {
			if ch.Index < 0 {
				continue
			}
			res.Total++
			validIndices[ch.Index] = struct{}{}
			targetPath := filepath.Join(itemDir, strconv.Itoa(ch.Index)+".jpg")
			if info, err := os.Stat(targetPath); err == nil && !info.IsDir() && info.ModTime().Unix() >= item.FileMtime {
				res.Existing++
				continue
			}
			pending = append(pending, ch)
		}

		pruneStaleChapterStills(itemDir, validIndices)

		if len(pending) == 0 {
			continue
		}
		if len(work) >= maxChapterThumbItemsPerRun {
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

			if err := s.extractChapterStill(ctx, w.item.FilePath, seekSec, isHDR, tmpPath); err != nil {
				_ = os.Remove(tmpPath)
				s.logger.Debug("chapter thumbnail extraction failed", "item_id", w.item.ID, "chapter", ch.Index, "error", err)
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

func (s *Service) extractChapterStill(ctx context.Context, filePath, seekSec string, isHDR bool, tmpPath string) error {
	runFFmpeg := func(vf string) error {
		cmdCtx, cancel := context.WithTimeout(ctx, chapterThumbTimeout)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, s.ffmpegBin,
			"-nostdin",
			"-loglevel", "error",
			"-threads", "1",
			"-ss", seekSec,
			"-i", filePath,
			"-map", "0:v:0",
			"-frames:v", "1",
			"-vf", vf,
			"-q:v", "4",
			"-f", "image2",
			"-y",
			tmpPath,
		)
		if err := cmd.Run(); err != nil {
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
		if !strings.HasSuffix(name, ".jpg") {
			continue
		}
		stem := strings.TrimSuffix(name, ".jpg")
		idx, err := strconv.Atoi(stem)
		if err != nil || strconv.Itoa(idx) != stem {
			continue
		}
		if _, ok := validIndices[idx]; !ok {
			_ = os.Remove(filepath.Join(itemDir, name))
		}
	}
}
