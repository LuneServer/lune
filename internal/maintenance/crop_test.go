package maintenance

import (
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

func TestMergeCropSamples(t *testing.T) {
	letter := cropRect{W: 1920, H: 800, X: 0, Y: 140}
	tests := []struct {
		name    string
		samples []cropRect
		want    cropRect
		ok      bool
	}{
		{"letterbox", []cropRect{letter, letter, letter}, letter, true},
		{"pillarbox", []cropRect{{1440, 1080, 240, 0}, {1440, 1080, 240, 0}}, cropRect{1440, 1080, 240, 0}, true},
		{"full frame untouched", []cropRect{{1920, 1080, 0, 0}, {1920, 1080, 0, 0}}, cropRect{}, false},
		// A dark scene reports a smaller area; the bright samples win (no over-crop).
		{"dark scene does not over-crop", []cropRect{letter, {1200, 400, 360, 340}, letter}, letter, true},
		// Mixed aspect (e.g. IMAX sections) keeps the largest picture.
		{"mixed aspect keeps largest", []cropRect{letter, {1920, 1080, 0, 0}}, cropRect{}, false},
		{"thin slivers ignored", []cropRect{{1920, 1072, 0, 4}, {1920, 1072, 0, 4}}, cropRect{}, false},
		{"implausibly small picture ignored", []cropRect{{400, 300, 760, 390}, {400, 300, 760, 390}}, cropRect{}, false},
		{"too few samples", []cropRect{letter}, cropRect{}, false},
		{"odd values made even", []cropRect{{1920, 801, 0, 139}, {1920, 801, 0, 139}}, cropRect{1920, 800, 0, 140}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mergeCropSamples(tc.samples, 1920, 1080)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("got %+v ok=%v, want %+v ok=%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// generateStillFor builds a 60s clip from a lavfi graph, indexes it with two chapters, runs the
// chapter still task, and returns the first still's dimensions and the recorded crop.
func generateStillFor(t *testing.T, graph string) (int, int, string) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	mediaRoot := t.TempDir()
	videoPath := filepath.Join(mediaRoot, "clip.mkv")
	if out, err := exec.Command(ffmpegPath, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", graph,
		"-t", "60", "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-pix_fmt", "yuv420p", "-y", videoPath).CombinedOutput(); err != nil {
		t.Skipf("clip generation unavailable: %v (%s)", err, out)
	}
	info, _ := os.Stat(videoPath)
	if err := db.CreateLibrary(ctx, store.Library{ID: "lib-1", Name: "Movies", Kind: "movies", RootPath: mediaRoot}); err != nil {
		t.Fatal(err)
	}
	chaptersJSON, _ := json.Marshal([]metadata.ChapterInfo{{Index: 0, StartMS: 0, EndMS: 30000}, {Index: 1, StartMS: 30000, EndMS: 60000}})
	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID: "item-crop", LibraryID: "lib-1", Kind: "movie", Title: "Clip", SortTitle: "Clip",
		FilePath: videoPath, FileMtime: info.ModTime().UnixNano(), VideoHDR: "sdr", ChaptersJSON: string(chaptersJSON),
	}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ctx, db, Options{DataDir: dataDir})
	res, err := svc.generateChapterThumbnails(ctx, nil)
	if err != nil || res.Generated != 2 {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}
	itemDir := filepath.Join(dataDir, "artwork", "chapters", "item-crop")
	f, err := os.Open(filepath.Join(itemDir, "0.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height, readChapterStillsMeta(itemDir).Crop
}

func TestChapterStills_CropLetterbox(t *testing.T) {
	w, h, crop := generateStillFor(t, "testsrc2=size=1920x800:rate=24,pad=1920:1080:0:140:black")
	if crop != "crop=1920:800:0:140" || w != 480 || h != 200 {
		t.Fatalf("letterboxed still = %dx%d crop=%q, want 480x200 crop=1920:800:0:140", w, h, crop)
	}
}

func TestChapterStills_CropPillarbox(t *testing.T) {
	w, h, crop := generateStillFor(t, "testsrc2=size=1440x1080:rate=24,pad=1920:1080:240:0:black")
	if crop != "crop=1440:1080:240:0" || w != 480 || h != 360 {
		t.Fatalf("pillarboxed still = %dx%d crop=%q, want 480x360", w, h, crop)
	}
}

func TestChapterStills_FullFrameUntouched(t *testing.T) {
	w, h, crop := generateStillFor(t, "testsrc2=size=1920x1080:rate=24")
	if crop != "" || w != 480 || h != 270 {
		t.Fatalf("full-frame still = %dx%d crop=%q, want 480x270 and no crop", w, h, crop)
	}
}
