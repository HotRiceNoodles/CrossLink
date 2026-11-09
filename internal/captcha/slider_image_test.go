package captcha

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pngEncodeHelper(buf *bytes.Buffer, img image.Image) error {
	return png.Encode(buf, img)
}

func writeFileHelper(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func sceneForTest(t *testing.T) *image.RGBA {
	t.Helper()
	img := renderScene(300, 150, seededSceneRNG(99))
	require.Equal(t, 300, img.Bounds().Dx())
	return img
}

func TestRenderSlider_FormatsAndDims(t *testing.T) {
	cfg := DefaultSliderConfig()
	scene := sceneForTest(t)
	bgB64, pieceB64 := renderSlider(scene, cfg, 200, 60)

	bgRaw, err := base64.StdEncoding.DecodeString(bgB64)
	require.NoError(t, err)
	bgImg, format, err := image.Decode(bytes.NewReader(bgRaw))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format, "background should be JPEG (photo-like, no alpha)")
	assert.Equal(t, 300, bgImg.Bounds().Dx())
	assert.Equal(t, 150, bgImg.Bounds().Dy())

	pieceRaw, err := base64.StdEncoding.DecodeString(pieceB64)
	require.NoError(t, err)
	pieceImg, pFormat, err := image.Decode(bytes.NewReader(pieceRaw))
	require.NoError(t, err)
	assert.Equal(t, "png", pFormat, "piece needs alpha → PNG")
	assert.Equal(t, cfg.PieceSize, pieceImg.Bounds().Dx())
	assert.Equal(t, cfg.PieceSize, pieceImg.Bounds().Dy())
}

// TestRenderPiece_JigsawAlpha pins the silhouette: opaque body + tabs,
// transparent corners, and a stroke ring.
func TestRenderPiece_JigsawAlpha(t *testing.T) {
	cfg := DefaultSliderConfig() // piece 44, inset ~6
	n := cfg.PieceSize
	inset := jigsawInset(n)
	cornerR := inset * 0.65
	scene := sceneForTest(t)
	piece := renderPiece(scene, n, 100, 60, float64(n), inset, cornerR)
	require.NotNil(t, piece)

	mustOpaque := [][2]int{
		{n / 2, n / 2},                   // body centre
		{n / 2, 1},                       // top tab (circle reaches y=0)
		{1, n / 2},                       // left tab
		{int(inset) + 2, int(inset) + 2}, // body interior near corner
	}
	for _, p := range mustOpaque {
		assert.Equal(t, uint8(255), piece.RGBAAt(p[0], p[1]).A, "pixel (%d,%d) must be opaque", p[0], p[1])
	}
	mustTransparent := [][2]int{
		{0, 0},         // above body, left of top tab
		{n - 1, 0},     // top-right, outside body and tab
		{n - 1, n - 1}, // bottom-right corner
		{0, n - 1},     // bottom-left, below left tab
	}
	for _, p := range mustTransparent {
		assert.Equal(t, uint8(0), piece.RGBAAt(p[0], p[1]).A, "pixel (%d,%d) must be transparent", p[0], p[1])
	}
}

// TestDrawGap_ConfinedToShape: pixels outside the gap's bounding box must be
// untouched by the gap rendering.
func TestDrawGap_ConfinedToShape(t *testing.T) {
	cfg := DefaultSliderConfig()
	n := cfg.PieceSize
	inset := jigsawInset(n)
	cornerR := inset * 0.65
	scene := sceneForTest(t)

	bg := image.NewRGBA(scene.Bounds())
	copyRGBA(bg, scene)
	gapX, gapY := 200, 60
	drawGap(bg, n, gapX, gapY, float64(n), inset, cornerR)

	margin := int(inset + strokeWidth + 2)
	changedOutside := 0
	for y := 0; y < bg.Bounds().Dy(); y++ {
		for x := 0; x < bg.Bounds().Dx(); x++ {
			inGapBox := x >= gapX-margin && x <= gapX+n+margin && y >= gapY-margin && y <= gapY+n+margin
			if !inGapBox && bg.RGBAAt(x, y) != scene.RGBAAt(x, y) {
				changedOutside++
			}
		}
	}
	assert.Zero(t, changedOutside, "gap rendering must not touch pixels outside the shape bbox")

	// And inside the shape the veil must be visible: centre pixel lightened.
	centre := scene.RGBAAt(gapX+n/2, gapY+n/2)
	after := bg.RGBAAt(gapX+n/2, gapY+n/2)
	assert.True(t, after.R > centre.R || after.G > centre.G || after.B > centre.B,
		"gap fill should lighten the covered pixel (before r=%d after r=%d)", centre.R, after.R)
}

// TestRenderSlider_PayloadSize pins the transport win from the JPEG switch:
// a photo-like 300×150 PNG background is typically >100KB of base64, the
// JPEG payload must stay comfortably under the old size.
func TestRenderSlider_PayloadSize(t *testing.T) {
	cfg := DefaultSliderConfig()
	scene := sceneForTest(t)
	bgB64, pieceB64 := renderSlider(scene, cfg, 200, 60)
	t.Logf("payload sizes: bg=%d bytes, piece=%d bytes (base64)", len(bgB64), len(pieceB64))
	assert.Less(t, len(bgB64), 45_000, "bg base64 payload should stay small (JPEG)")
	assert.Less(t, len(pieceB64), 16_000, "piece base64 payload should stay small (44px PNG)")
}

func copyRGBA(dst, src *image.RGBA) {
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			dst.SetRGBA(x, y, src.RGBAAt(x, y))
		}
	}
}

func TestSliderIssue_BackgroundMimeAndPhotos(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	var buf bytes.Buffer
	require.NoError(t, pngEncodeHelper(&buf, img))
	require.NoError(t, writeFileHelper(filepath.Join(dir, "photo.png"), buf.Bytes()))

	p, _ := testSliderProvider(t)
	p.photos = loadPhotoLibrary(dir, 300, 150)
	require.NotNil(t, p.photos)
	ch, err := p.Issue(context.Background(), "1.2.3.4", "login")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", ch.BGMime)
	assert.NotEmpty(t, ch.BGImage)
	assert.NotEmpty(t, ch.PuzzleImage)
}
