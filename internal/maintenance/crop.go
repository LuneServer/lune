package maintenance

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Black bar (letterbox / pillarbox) detection for chapter stills.
//
// FFmpeg's cropdetect reports the non-black area of a few keyframes at several points in the
// title. A dark scene makes cropdetect report a *smaller* area than the real picture, so the
// samples are combined into their bounding box (the largest picture area seen): a single
// normally lit sample is enough to stop over-cropping. Titles without bars, or with bars
// thinner than cropMinBarFraction of the frame, are left untouched.
const (
	cropSampleTimeout  = 20 * time.Second
	cropMinBarFraction = 0.02 // ignore slivers (encoder padding, overscan) under 2% of an axis
	cropMinAreaFrac    = 0.40 // a detected picture smaller than this is treated as unreliable
	cropMinSamples     = 2
)

// cropSamplePoints are the fractions of the runtime sampled for bar detection; they avoid the
// first/last few minutes where logos, fades and credits are most often black.
var cropSamplePoints = []float64{0.15, 0.30, 0.45, 0.60, 0.75, 0.90}

var (
	cropLineRE   = regexp.MustCompile(`crop=(\d+):(\d+):(\d+):(\d+)`)
	videoDimsRE  = regexp.MustCompile(`Video:.*?[ ,](\d{2,5})x(\d{2,5})[ ,\[]`)
	errNoSamples = fmt.Errorf("not enough cropdetect samples")
)

// cropRect is a picture area in source pixels.
type cropRect struct{ W, H, X, Y int }

func (r cropRect) filter() string { return fmt.Sprintf("crop=%d:%d:%d:%d", r.W, r.H, r.X, r.Y) }

// mergeCropSamples combines per-sample detections into the crop to apply, or ok=false when the
// frame should be left as is. frameW/frameH are the decoded frame dimensions.
func mergeCropSamples(samples []cropRect, frameW, frameH int) (cropRect, bool) {
	if len(samples) < cropMinSamples || frameW <= 0 || frameH <= 0 {
		return cropRect{}, false
	}
	x1, y1 := frameW, frameH
	x2, y2 := 0, 0
	for _, s := range samples {
		if s.W <= 0 || s.H <= 0 {
			continue
		}
		x1 = min(x1, s.X)
		y1 = min(y1, s.Y)
		x2 = max(x2, s.X+s.W)
		y2 = max(y2, s.Y+s.H)
	}
	x1, y1 = max(x1, 0), max(y1, 0)
	x2, y2 = min(x2, frameW), min(y2, frameH)
	if x2 <= x1 || y2 <= y1 {
		return cropRect{}, false
	}
	// Leave an axis alone when its bars are too thin to matter.
	if float64(frameW-(x2-x1)) < cropMinBarFraction*float64(frameW) {
		x1, x2 = 0, frameW
	}
	if float64(frameH-(y2-y1)) < cropMinBarFraction*float64(frameH) {
		y1, y2 = 0, frameH
	}
	r := cropRect{W: x2 - x1, H: y2 - y1, X: x1, Y: y1}
	// Keep even dimensions/offsets for 4:2:0 sources.
	r.X += r.X & 1
	r.Y += r.Y & 1
	r.W -= r.W & 1
	r.H -= r.H & 1
	if r.X+r.W > frameW {
		r.W = (frameW - r.X) &^ 1
	}
	if r.Y+r.H > frameH {
		r.H = (frameH - r.Y) &^ 1
	}
	if r.W == frameW && r.H == frameH {
		return cropRect{}, false
	}
	if float64(r.W*r.H) < cropMinAreaFrac*float64(frameW*frameH) {
		return cropRect{}, false
	}
	return r, true
}

// detectBlackBars samples the title and returns the crop filter to apply ("" for none).
// durationMS bounds the sample points; it is approximate (e.g. the last chapter's end).
func (s *Service) detectBlackBars(ctx context.Context, filePath string, durationMS int64) (string, error) {
	if durationMS <= 0 {
		return "", nil
	}
	var (
		samples        []cropRect
		frameW, frameH int
	)
	for _, frac := range cropSamplePoints {
		if s.isBusyForChapterThumbnails(ctx) {
			return "", errChapterBusy
		}
		at := fmt.Sprintf("%.3f", float64(durationMS)*frac/1000.0)
		r, w, h, err := s.cropdetectAt(ctx, filePath, at)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		if w > 0 && h > 0 {
			frameW, frameH = w, h
		}
		samples = append(samples, r)
	}
	if len(samples) < cropMinSamples {
		return "", errNoSamples
	}
	if r, ok := mergeCropSamples(samples, frameW, frameH); ok {
		return r.filter(), nil
	}
	return "", nil
}

// cropdetectAt runs cropdetect over a few keyframes from the given position and returns the
// final detection plus the decoded frame size.
func (s *Service) cropdetectAt(ctx context.Context, filePath, at string) (cropRect, int, int, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, cropSampleTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, s.ffmpegBin,
		"-nostdin", "-hide_banner", "-nostats",
		"-loglevel", "info",
		"-threads", "1",
		"-filter_threads", "1",
		"-skip_frame", "nokey",
		"-ss", at,
		"-i", filePath,
		"-map", "0:v:0",
		"-an", "-sn", "-dn",
		"-frames:v", "4",
		// limit is a fraction of the bit depth's range, so 8-bit and 10-bit HDR behave alike.
		"-vf", "cropdetect=limit=0.094:round=2:reset=0",
		"-f", "null", "-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := runBackground(cmd)

	var (
		last           *cropRect
		frameW, frameH int
	)
	sc := bufio.NewScanner(&stderr)
	for sc.Scan() {
		line := sc.Text()
		if frameW == 0 {
			if m := videoDimsRE.FindStringSubmatch(line); m != nil {
				frameW, _ = strconv.Atoi(m[1])
				frameH, _ = strconv.Atoi(m[2])
			}
		}
		if m := cropLineRE.FindStringSubmatch(line); m != nil {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			x, _ := strconv.Atoi(m[3])
			y, _ := strconv.Atoi(m[4])
			last = &cropRect{W: w, H: h, X: x, Y: y}
		}
	}
	if last == nil {
		if runErr != nil {
			return cropRect{}, 0, 0, runErr
		}
		return cropRect{}, 0, 0, fmt.Errorf("no cropdetect output")
	}
	return *last, frameW, frameH, nil
}
