package captcha

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	rand "math/rand/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seededSceneRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, 0xC0FFEE))
}

func TestRenderScene_DeterministicPerSeed(t *testing.T) {
	a := renderScene(300, 150, seededSceneRNG(42))
	b := renderScene(300, 150, seededSceneRNG(42))
	require.Equal(t, a.Bounds(), b.Bounds())
	for y := 0; y < 150; y++ {
		for x := 0; x < 300; x++ {
			assert.Equal(t, a.RGBAAt(x, y), b.RGBAAt(x, y), "same seed must render identical pixels at (%d,%d)", x, y)
		}
	}
}

func TestRenderScene_SeedVaries(t *testing.T) {
	a := renderScene(300, 150, seededSceneRNG(1))
	b := renderScene(300, 150, seededSceneRNG(2))
	diff := 0
	for y := 0; y < 150; y++ {
		for x := 0; x < 300; x++ {
			if a.RGBAAt(x, y) != b.RGBAAt(x, y) {
				diff++
			}
		}
	}
	assert.Greater(t, diff, 1000, "different seeds should produce visibly different scenes")
}

// TestSceneGens_Render checks every scene family renders a full, opaque,
// non-uniform canvas — including at awkward small sizes (guards div-by-zero
// and IntN-panics in the generators).
func TestSceneGens_Render(t *testing.T) {
	sizes := [][2]int{{300, 150}, {60, 30}, {320, 160}}
	for _, gen := range sceneGens {
		for _, sz := range sizes {
			img := image.NewRGBA(image.Rect(0, 0, sz[0], sz[1]))
			gen(img, seededSceneRNG(uint64(sz[0])))
			// variance check
			var sumR, sumR2 float64
			n := float64(sz[0] * sz[1])
			for y := 0; y < sz[1]; y++ {
				for x := 0; x < sz[0]; x++ {
					c := img.RGBAAt(x, y)
					require.Equal(t, uint8(255), c.A, "scene pixels must be opaque")
					sumR += float64(c.R)
					sumR2 += float64(c.R) * float64(c.R)
				}
			}
			variance := sumR2/n - (sumR/n)*(sumR/n)
			assert.Greater(t, variance, 100.0, "scene should have color variation, got flat image")
		}
	}
}

func TestBoxBlur_PreservesBounds(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			if x < 20 {
				src.SetRGBA(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				src.SetRGBA(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	out := boxBlur(src, 3)
	require.Equal(t, src.Bounds(), out.Bounds())
	// The seam must soften: some pixel between the halves mixes both colors.
	mixed := false
	for y := 0; y < 20 && !mixed; y++ {
		for x := 16; x < 24; x++ {
			c := out.RGBAAt(x, y)
			if c.R > 20 && c.B > 20 {
				mixed = true
				break
			}
		}
	}
	assert.True(t, mixed, "blur should mix colors across the seam")
}

func TestScaleCrop_AspectAndSize(t *testing.T) {
	// 200×100 source cropped to 2:1 already; scale to 100×50.
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			src.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 7, 255})
		}
	}
	out := scaleCrop(src, 100, 50)
	require.Equal(t, 100, out.Bounds().Dx())
	require.Equal(t, 50, out.Bounds().Dy())

	// Wider target from a narrow source must not distort: crop then scale.
	tall := image.NewRGBA(image.Rect(0, 0, 10, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 10; x++ {
			tall.SetRGBA(x, y, color.RGBA{90, 90, 90, 255})
		}
	}
	out2 := scaleCrop(tall, 100, 50)
	require.Equal(t, 100, out2.Bounds().Dx())
	require.Equal(t, 50, out2.Bounds().Dy())
}

func TestPhotoLibrary_MissingDirFallsBack(t *testing.T) {
	lib := loadPhotoLibrary("definitely/not/a/dir", 300, 150)
	assert.Nil(t, lib, "unreadable dir must yield nil so the slider uses procedural scenes")
}

func TestPhotoLibrary_LoadsImages(t *testing.T) {
	dir := t.TempDir()
	// tiny 4×2 PNG
	img := image.NewRGBA(image.Rect(0, 0, 4, 2))
	var buf bytes.Buffer
	require.NoError(t, pngEncodeHelper(&buf, img))
	require.NoError(t, writeFileHelper(dir+"/a.png", buf.Bytes()))

	lib := loadPhotoLibrary(dir, 300, 150)
	require.NotNil(t, lib)
	picked := lib.pick(seededSceneRNG(7), 300, 150)
	assert.Equal(t, 300, picked.Bounds().Dx())
	assert.Equal(t, 150, picked.Bounds().Dy())
}
