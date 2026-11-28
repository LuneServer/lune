package maintenance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png" // DecodeConfig for intermediate sheet dimensions
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

// Trickplay (scrubbing preview) generation aims for the smallest storage and server cost at
// the best accuracy the source allows:
//
// Cost
//   - one FFmpeg pass per item that decodes keyframes only (-skip_frame nokey): roughly one
//     I-frame decode per GOP instead of every frame;
//   - single-threaded decode, filtering and encoding at background priority (renice 15);
//   - letterbox/pillarbox black bars are cropped (shared detection with chapter stills; the
//     crop chapter stills already detected for the same file version is reused), which cut a
//     2.40:1 sheet by ~15% and keeps bars out of the preview;
//   - downscaling uses bicubic (a properly filtered downscale) rather than fast_bilinear: on a
//     12x shrink fast_bilinear keeps grain as aliased noise, which doubled sheet size in testing
//     at no measurable decode-time saving;
//   - the FFmpeg process is SIGSTOP'd while any transcode or library scan is active and resumed
//     automatically afterwards, so it never competes with playback;
//   - items are only rebuilt when the file, the settings, or the best available format change.
//
// Size
//   - tiles are packed 10x10 into sprite sheets encoded as AVIF (SVT-AV1 / libaom, CRF 35),
//     which in testing matched or beat JPEG q5 quality (SSIM); with the bicubic downscale and
//     de-duplication, sheets came out 55-66% smaller than the first JPEG pipeline. Servers
//     whose FFmpeg lacks an AV1 encoder fall back to WebP, then JPEG;
//   - near-identical consecutive thumbnails (static shots, black frames, credits, or the same
//     keyframe sampled twice when the GOP is longer than the interval) are dropped with
//     mpdecimate. The manifest then lists each tile's timestamp, so a dense interval costs no
//     storage where the picture doesn't change.
//
// Accuracy
//   - each tile is the keyframe at or before its slot, so precision is the smaller of the
//     interval and the source's keyframe spacing.
const (
	trickplayManifestVersion = 3 // v3: black bars cropped, tiles fit within a 16:9 box
	trickplayColumns         = 10
	trickplayRows            = 10
	// Tiles fit inside width x (width*9/16): letterboxed (cropped) titles get shorter tiles and
	// pillarboxed 4:3 titles narrower ones, so cropping never makes a tile larger than 16:9.
	trickplayFitScale = "scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=bicubic"
	trickplayHDRChain = "zscale=t=linear:npl=100,format=gbrpf32le,tonemap=hable:desat=0,zscale=p=bt709:t=bt709:m=bt709:r=tv,format=yuv420p"
)

// Tunable in tests.
var (
	trickplayBusyPoll       = 2 * time.Second
	trickplayMaxPausePerRun = 6 * time.Hour // give up for this run if playback never stops
	// trickplayMaxCPUPerItem is the CPU time one FFmpeg call may use. It is measured from the
	// kernel where possible (Linux), so a low-priority job starved by other work is not
	// mistaken for a stuck one; elsewhere unpaused wall time stands in for it.
	trickplayMaxCPUPerItem = 90 * time.Minute
	// trickplayMaxWallPerItem bounds unpaused wall time when CPU time is measured, catching a
	// job stalled on I/O (e.g. a hung network mount). Hitting it skips the item for this run
	// instead of marking it failed.
	trickplayMaxWallPerItem = 6 * time.Hour
	// forceTrickplayFormat pins the sheet format (tests only); empty means probe.
	forceTrickplayFormat = ""
)

var (
	errTrickplayBusyTimeout = errors.New("trickplay generation paused too long")
	errTrickplayStalled     = errors.New("trickplay generation made too little progress")
	showinfoFrameRE         = regexp.MustCompile(`Parsed_showinfo.*\bn:\s*(\d+).*\bpts_time:\s*(-?[0-9.]+)`)
)

// trickplayEncoder is a sheet encoding in preference order (smallest first).
type trickplayEncoder struct {
	Format string   // file extension & manifest format: avif | webp | jpg
	Args   []string // FFmpeg output args (codec, quality, muxer)
}

var trickplayEncoders = []trickplayEncoder{
	{Format: "avif", Args: []string{"-c:v", "libsvtav1", "-preset", "8", "-crf", "35", "-svtav1-params", "lp=1", "-pix_fmt", "yuv420p", "-frames:v", "1", "-f", "avif"}},
	{Format: "avif", Args: []string{"-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "35", "-b:v", "0", "-row-mt", "0", "-threads", "1", "-pix_fmt", "yuv420p", "-frames:v", "1", "-f", "avif"}},
	{Format: "webp", Args: []string{"-c:v", "libwebp", "-quality", "60", "-compression_level", "4", "-frames:v", "1", "-f", "webp"}},
	{Format: "jpg", Args: []string{"-c:v", "mjpeg", "-q:v", "5", "-frames:v", "1", "-f", "image2"}},
}

// trickplaySheetEncoder probes once for the smallest sheet format this FFmpeg can produce.
func (s *Service) trickplaySheetEncoder() trickplayEncoder {
	s.tpEncOnce.Do(func() {
		s.tpEnc = trickplayEncoders[len(trickplayEncoders)-1]
		dir, err := os.MkdirTemp("", "lune-trickplay-probe-")
		if err != nil {
			return
		}
		defer os.RemoveAll(dir)
		for _, enc := range trickplayEncoders {
			if forceTrickplayFormat != "" && enc.Format != forceTrickplayFormat {
				continue
			}
			out := filepath.Join(dir, "probe."+enc.Format)
			args := append([]string{"-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x180:duration=1"}, enc.Args...)
			args = append(args, "-y", out)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := exec.CommandContext(ctx, s.ffmpegBin, args...).Run()
			cancel()
			if info, statErr := os.Stat(out); err == nil && statErr == nil && info.Size() > 0 {
				s.tpEnc = enc
				s.logger.Info("trickplay sheet format selected", "format", enc.Format, "encoder", enc.Args[1])
				return
			}
		}
	})
	return s.tpEnc
}

type trickplayResult struct {
	Generated     int
	Existing      int
	Total         int
	MissingFiles  int
	Unextractable int
	SkippedBusy   bool
	Bytes         int64 // total on-disk size of all current trickplay sets
}

const trickplayFailedMarker = "failed"

// trickplayUpToDate reports whether an item's manifest matches the current file, settings and format.
func trickplayUpToDate(m metadata.TrickplayManifest, item store.TrickplaySourceItem, cfg Config, format string) bool {
	return m.Version == trickplayManifestVersion &&
		m.SourceMtime == item.FileMtime &&
		m.IntervalMS == int64(cfg.TrickplayIntervalSeconds)*1000 &&
		m.ConfiguredWidth == cfg.TrickplayWidth &&
		m.Format == format
}

// trickplayFailedFor reports whether a failure marker records this exact file version and settings.
func trickplayFailedFor(itemDir string, item store.TrickplaySourceItem, cfg Config, format string) bool {
	raw, err := os.ReadFile(filepath.Join(itemDir, trickplayFailedMarker))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(raw)) == trickplayFailureKey(item, cfg, format)
}

func trickplayFailureKey(item store.TrickplaySourceItem, cfg Config, format string) string {
	return fmt.Sprintf("%d:%d:%d:%s", item.FileMtime, cfg.TrickplayIntervalSeconds, cfg.TrickplayWidth, format)
}

func (s *Service) generateTrickplay(ctx context.Context, cfg Config, onProgress func(done, total int, detail string)) (trickplayResult, error) {
	var res trickplayResult
	if s.ffmpegBin == "" {
		return res, nil
	}
	items, err := s.store.ListItemsForTrickplay(ctx)
	if err != nil {
		return res, err
	}
	enc := s.trickplaySheetEncoder()

	var work []store.TrickplaySourceItem
	for _, item := range items {
		res.Total++
		itemDir := metadata.TrickplayDir(s.dataDir, item.ID)
		if m, err := metadata.ReadTrickplayManifest(s.dataDir, item.ID); err == nil && trickplayUpToDate(m, item, cfg, enc.Format) {
			res.Existing++
			res.Bytes += dirSize(itemDir)
			continue
		}
		if trickplayFailedFor(itemDir, item, cfg, enc.Format) {
			res.Unextractable++
			continue
		}
		if _, err := os.Stat(item.FilePath); err != nil {
			res.MissingFiles++
			continue
		}
		work = append(work, item)
	}

	pausedBudget := trickplayMaxPausePerRun
	for i, item := range work {
		if ctx.Err() != nil {
			return res, nil
		}
		label := strings.TrimSpace(item.Title)
		if label == "" {
			label = filepath.Base(item.FilePath)
		}
		if onProgress != nil {
			onProgress(i, len(work), fmt.Sprintf("%s (%d/%d)", label, i+1, len(work)))
		}
		err := s.buildTrickplay(ctx, item, cfg, enc, &pausedBudget, func(paused bool) {
			if onProgress == nil {
				return
			}
			detail := fmt.Sprintf("%s (%d/%d)", label, i+1, len(work))
			if paused {
				detail = "Paused while media is playing or a scan runs: " + detail
			}
			onProgress(i, len(work), detail)
		})
		switch {
		case err == nil:
			res.Generated++
			res.Existing++
			res.Bytes += dirSize(metadata.TrickplayDir(s.dataDir, item.ID))
		case ctx.Err() != nil:
			return res, nil
		case errors.Is(err, errTrickplayBusyTimeout):
			res.SkippedBusy = true
			return res, nil
		case errors.Is(err, errTrickplayStalled):
			// Starved or stalled rather than broken: no failed marker, retry next run.
			s.logger.Warn("trickplay generation stalled; will retry next run", "item_id", item.ID, "error", err)
		default:
			s.logger.Debug("trickplay generation failed", "item_id", item.ID, "error", err)
			itemDir := metadata.TrickplayDir(s.dataDir, item.ID)
			if mkErr := os.MkdirAll(itemDir, 0o755); mkErr == nil {
				_ = os.WriteFile(filepath.Join(itemDir, trickplayFailedMarker), []byte(trickplayFailureKey(item, cfg, enc.Format)), 0o644)
			}
			res.Unextractable++
		}
		if onProgress != nil {
			onProgress(i+1, len(work), fmt.Sprintf("%s (%d/%d)", label, i+1, len(work)))
		}
	}
	return res, nil
}

// buildTrickplay renders one item's sheets into a sibling build directory and swaps it in.
// Stage 1 samples, de-duplicates and tiles keyframes into lossless PNG sheets; stage 2 encodes
// each sheet into the selected compact format.
func (s *Service) buildTrickplay(ctx context.Context, item store.TrickplaySourceItem, cfg Config, enc trickplayEncoder, pausedBudget *time.Duration, onPause func(bool)) error {
	itemDir := metadata.TrickplayDir(s.dataDir, item.ID)
	buildDir := itemDir + ".building"
	_ = os.RemoveAll(buildDir)
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(buildDir)

	crop, err := s.trickplayCrop(ctx, item, pausedBudget, onPause)
	if err != nil {
		return err
	}
	prefix := ""
	if crop != "" {
		prefix = crop + ","
	}
	fit := fmt.Sprintf(trickplayFitScale, cfg.TrickplayWidth, (cfg.TrickplayWidth*9/16)&^1)

	isHDR := item.VideoHDR != "" && !strings.EqualFold(item.VideoHDR, "sdr")
	var stampsMS []int64
	if isHDR {
		stampsMS, err = s.renderTrickplayTiles(ctx, item.FilePath, buildDir, prefix+trickplayHDRChain+","+fit, cfg, pausedBudget, onPause)
	}
	if !isHDR || (err != nil && ctx.Err() == nil && !errors.Is(err, errTrickplayBusyTimeout)) {
		clearDir(buildDir)
		stampsMS, err = s.renderTrickplayTiles(ctx, item.FilePath, buildDir, prefix+fit, cfg, pausedBudget, onPause)
	}
	if err != nil {
		return err
	}
	count := len(stampsMS)
	if count == 0 {
		return fmt.Errorf("ffmpeg produced no thumbnails")
	}

	perSheet := trickplayColumns * trickplayRows
	sheets := (count + perSheet - 1) / perSheet
	f, err := os.Open(filepath.Join(buildDir, "0.png"))
	if err != nil {
		return fmt.Errorf("first sheet missing: %w", err)
	}
	dim, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("decode sheet: %w", err)
	}
	// tile always renders the full grid (padding unused cells), so geometry divides evenly.
	thumbW := dim.Width / trickplayColumns
	thumbH := dim.Height / trickplayRows
	if thumbW <= 0 || thumbH <= 0 {
		return fmt.Errorf("invalid sheet geometry %dx%d", dim.Width, dim.Height)
	}

	// Stage 2: encode each lossless sheet into the compact format.
	for i := 0; i < sheets; i++ {
		src := filepath.Join(buildDir, strconv.Itoa(i)+".png")
		dst := filepath.Join(buildDir, strconv.Itoa(i)+"."+enc.Format)
		args := append([]string{"-nostdin", "-loglevel", "error", "-threads", "1", "-i", src}, enc.Args...)
		args = append(args, "-y", dst)
		if err := s.runPausableFFmpeg(ctx, args, pausedBudget, onPause, nil); err != nil {
			return fmt.Errorf("encode sheet %d: %w", i, err)
		}
		if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
			return fmt.Errorf("sheet %d empty", i)
		}
		_ = os.Remove(src)
	}

	manifest := metadata.TrickplayManifest{
		Version:         trickplayManifestVersion,
		Format:          enc.Format,
		IntervalMS:      int64(cfg.TrickplayIntervalSeconds) * 1000,
		ThumbWidth:      thumbW,
		ThumbHeight:     thumbH,
		Columns:         trickplayColumns,
		Rows:            trickplayRows,
		ThumbCount:      count,
		SheetCount:      sheets,
		SourceMtime:     item.FileMtime,
		KeyframesOnly:   true,
		ConfiguredWidth: cfg.TrickplayWidth,
		Crop:            crop,
		GeneratedAt:     time.Now().Unix(),
	}
	if !uniformStamps(stampsMS, manifest.IntervalMS) {
		manifest.TimestampsMS = stampsMS
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(buildDir, metadata.TrickplayManifestName), b, 0o644); err != nil {
		return err
	}
	_ = os.RemoveAll(itemDir)
	return os.Rename(buildDir, itemDir)
}

// trickplayCrop returns the black-bar crop filter for an item ("" for none). It reuses the
// crop chapter stills recorded for the same file version, otherwise detects it, waiting
// (within the run's pause budget) while the server is busy.
func (s *Service) trickplayCrop(ctx context.Context, item store.TrickplaySourceItem, pausedBudget *time.Duration, onPause func(bool)) (string, error) {
	meta := readChapterStillsMeta(filepath.Join(s.dataDir, "artwork", "chapters", item.ID))
	if meta.Version == chapterStillsVersion && meta.SourceMtime == item.FileMtime {
		return meta.Crop, nil
	}
	for {
		if err := s.waitTrickplayIdle(ctx, pausedBudget, onPause); err != nil {
			return "", err
		}
		crop, err := s.detectBlackBars(ctx, item.FilePath, item.DurationMS)
		switch {
		case errors.Is(err, errChapterBusy):
			continue // playback started mid-detection; wait again
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			s.logger.Debug("black bar detection failed; keeping full frame", "item_id", item.ID, "error", err)
			return "", nil
		}
		return crop, nil
	}
}

// waitTrickplayIdle blocks while a scan or transcode is active, charging the wait to the
// run's pause budget.
func (s *Service) waitTrickplayIdle(ctx context.Context, pausedBudget *time.Duration, onPause func(bool)) error {
	if !s.isBusyForChapterThumbnails(ctx) {
		return ctx.Err()
	}
	onPause(true)
	start := time.Now()
	ticker := time.NewTicker(trickplayBusyPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if !s.isBusyForChapterThumbnails(ctx) {
			*pausedBudget -= time.Since(start)
			onPause(false)
			return nil
		}
		if *pausedBudget-time.Since(start) <= 0 {
			*pausedBudget = 0
			return errTrickplayBusyTimeout
		}
	}
}

// uniformStamps reports whether tile n sits at n*interval for every tile (no tiles dropped),
// in which case clients can index arithmetically and the timestamp list is omitted.
func uniformStamps(stamps []int64, intervalMS int64) bool {
	for i, ts := range stamps {
		if d := ts - int64(i)*intervalMS; d < -intervalMS/2 || d > intervalMS/2 {
			return false
		}
	}
	return true
}

func clearDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// renderTrickplayTiles runs the keyframe-only sampling pass into lossless PNG sheets and returns
// each kept tile's media timestamp (ms), in tile order.
func (s *Service) renderTrickplayTiles(ctx context.Context, filePath, outDir, scaleVF string, cfg Config, pausedBudget *time.Duration, onPause func(bool)) ([]int64, error) {
	// mpdecimate drops tiles nearly identical to the previous kept tile; showinfo reports the
	// survivors' timestamps.
	vf := fmt.Sprintf("fps=1/%d:round=down,%s,mpdecimate,showinfo,tile=%dx%d",
		cfg.TrickplayIntervalSeconds, scaleVF, trickplayColumns, trickplayRows)
	args := []string{
		"-nostdin", "-hide_banner", "-nostats",
		"-loglevel", "info",
		"-threads", "1",
		"-skip_frame", "nokey",
		"-i", filePath,
		"-map", "0:v:0",
		"-an", "-sn", "-dn",
		"-filter_threads", "1",
		"-vf", vf,
		"-fps_mode", "passthrough",
		"-c:v", "png", "-compression_level", "1",
		"-start_number", "0",
		"-f", "image2",
		"-y",
		filepath.Join(outDir, "%d.png"),
	}
	byIndex := map[int]int64{}
	var mu sync.Mutex
	err := s.runPausableFFmpeg(ctx, args, pausedBudget, onPause, func(line string) bool {
		m := showinfoFrameRE.FindStringSubmatch(line)
		if m == nil {
			return false
		}
		n, err1 := strconv.Atoi(m[1])
		sec, err2 := strconv.ParseFloat(m[2], 64)
		if err1 == nil && err2 == nil {
			mu.Lock()
			byIndex[n] = int64(sec*1000 + 0.5)
			mu.Unlock()
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	idx := make([]int, 0, len(byIndex))
	for n := range byIndex {
		idx = append(idx, n)
	}
	sort.Ints(idx)
	stamps := make([]int64, len(idx))
	for i, n := range idx {
		stamps[i] = byIndex[n]
	}
	return stamps, nil
}

// runBackground runs a short FFmpeg job (a chapter still or cropdetect sample) at background
// priority. These finish in seconds, so they skip runPausableFFmpeg's pause handling.
func runBackground(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	lowerProcessPriority(cmd)
	return cmd.Wait()
}

// runPausableFFmpeg runs FFmpeg at background priority, SIGSTOPping it while a scan or
// transcode is active. onLine may consume stderr lines (returning true); other lines are kept
// as error context.
func (s *Service) runPausableFFmpeg(ctx context.Context, args []string, pausedBudget *time.Duration, onPause func(bool), onLine func(string) bool) error {
	cmdCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, s.ffmpegBin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lowerProcessPriority(cmd)

	var (
		mu   sync.Mutex
		tail []string
	)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if onLine != nil && onLine(line) {
				continue
			}
			mu.Lock()
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
			mu.Unlock()
		}
	}()

	waitErr := make(chan error, 1)
	go func() {
		<-scanDone
		waitErr <- cmd.Wait()
	}()

	var (
		paused     bool
		pausedAt   time.Time
		activeWall time.Duration
		lastTick   = time.Now()
		budgetErr  error
	)
	ticker := time.NewTicker(trickplayBusyPoll)
	defer ticker.Stop()
	for {
		select {
		case err := <-waitErr:
			if paused {
				resumeProcess(cmd)
			}
			if budgetErr != nil {
				return budgetErr
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				mu.Lock()
				msg := strings.Join(tail, "; ")
				mu.Unlock()
				return fmt.Errorf("ffmpeg: %w (%s)", err, msg)
			}
			return nil
		case <-ticker.C:
			now := time.Now()
			busy := s.isBusyForChapterThumbnails(ctx)
			switch {
			case busy && !paused:
				if pauseProcess(cmd) {
					paused, pausedAt = true, now
					onPause(true)
				} else {
					// Can't pause on this platform: stop and let a later run retry.
					*pausedBudget = 0
					budgetErr = errTrickplayBusyTimeout
					cancel()
				}
			case !busy && paused:
				resumeProcess(cmd)
				*pausedBudget -= now.Sub(pausedAt)
				paused = false
				onPause(false)
			case paused && *pausedBudget-now.Sub(pausedAt) <= 0:
				resumeProcess(cmd)
				paused = false
				*pausedBudget = 0
				budgetErr = errTrickplayBusyTimeout
				cancel()
			case !paused:
				activeWall += now.Sub(lastTick)
				if cpu, ok := cpuTimeOf(cmd); ok {
					switch {
					case cpu > trickplayMaxCPUPerItem:
						budgetErr = fmt.Errorf("exceeded %s CPU budget", trickplayMaxCPUPerItem)
						cancel()
					case activeWall > trickplayMaxWallPerItem:
						budgetErr = fmt.Errorf("%w: %s CPU in %s", errTrickplayStalled, cpu.Round(time.Second), activeWall.Round(time.Second))
						cancel()
					}
				} else if activeWall > trickplayMaxCPUPerItem {
					budgetErr = fmt.Errorf("exceeded %s budget", trickplayMaxCPUPerItem)
					cancel()
				}
			}
			lastTick = now
		}
	}
}
