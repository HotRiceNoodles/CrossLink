package captcha

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	rand "math/rand/v2"
)

// TestWriteSliderSamples renders sample challenges per scene family (raw
// scene, background with gap, and the solved composite with the piece in
// place) into PNG files for visual review. Skipped unless
// CAPTCHA_WRITE_SAMPLES points at a directory; never part of CI.
func TestWriteSliderSamples(t *testing.T) {
	dir := os.Getenv("CAPTCHA_WRITE_SAMPLES")
	if dir == "" {
		t.Skip("set CAPTCHA_WRITE_SAMPLES=<dir> to write scene samples")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultSliderConfig() // 300×150, piece 44
	scenes := []struct {
		name string
		gen  sceneGen
	}{
		{"city", sceneDuskCity},
		{"mountains", sceneMountains},
		{"ocean", sceneOcean},
		{"geo", sceneGeoMesh},
		{"aurora", sceneAurora},
	}

	// Contact sheet (all scenes in one grid) for a single-pass visual review.
	sheet := image.NewRGBA(image.Rect(0, 0, cfg.BGWidth*2, cfg.BGHeight*len(scenes)))
	for row, s := range scenes {
		for col := 0; col < 2; col++ {
			rng := rand.New(rand.NewPCG(uint64(col*7919+13), 0xC0FFEE))
			img := image.NewRGBA(image.Rect(0, 0, cfg.BGWidth, cfg.BGHeight))
			s.gen(img, rng)
			vignettePass(img, 0.22)
			ditherPass(img, 3)
			draw.Draw(sheet, image.Rect(col*cfg.BGWidth, row*cfg.BGHeight,
				(col+1)*cfg.BGWidth, (row+1)*cfg.BGHeight), img, image.Point{}, draw.Src)
		}
	}
	writeImage(t, filepath.Join(dir, "contact_sheet.png"), sheet)

	for _, s := range scenes {
		for i := 0; i < 2; i++ {
			rng := rand.New(rand.NewPCG(uint64(i*7919+13), 0xC0FFEE))
			img := image.NewRGBA(image.Rect(0, 0, cfg.BGWidth, cfg.BGHeight))
			s.gen(img, rng)
			vignettePass(img, 0.22)
			ditherPass(img, 3)
			writePNG(t, filepath.Join(dir, "scene_"+s.name+"_"+string(rune('a'+i))+".png"), img)

			// Full challenge at a representative gap position, plus the
			// composite at the solved offset to review alignment.
			gapX, gapY := 196, 52
			bg, piece := renderSlider(img, cfg, gapX, gapY)
			bgImg := decodeB64(t, bg, "image/jpeg")
			pieceImg := decodeB64(t, piece, "image/png")
			writeImage(t, filepath.Join(dir, "bg_"+s.name+"_"+string(rune('a'+i))+".png"), bgImg)

			composite := image.NewRGBA(bgImg.Bounds())
			draw.Draw(composite, composite.Bounds(), bgImg, image.Point{}, draw.Src)
			draw.Draw(composite, image.Rect(gapX, gapY, gapX+cfg.PieceSize, gapY+cfg.PieceSize),
				pieceImg, image.Point{}, draw.Over)
			writeImage(t, filepath.Join(dir, "solved_"+s.name+"_"+string(rune('a'+i))+".png"), composite)
		}
	}
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	writeImage(t, path, img)
}

func writeImage(t *testing.T, path string, img image.Image) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", path)
}

func decodeB64(t *testing.T, b64, mime string) image.Image {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode (%s): %v", mime, err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("image decode (%s): %v", mime, err)
	}
	return img
}
