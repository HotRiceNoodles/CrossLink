package captcha

import (
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
)

// photoLibrary loads admin-supplied background photos from a directory
// (captcha.slider.backgrounds_dir, yaml-only). Photos are decoded once,
// centre-cropped and box-scaled into a cache; Issue rescales from the cache
// so runtime geometry changes (bg_width/bg_height are hot-settable) still
// work without re-reading disk. An empty or broken library is non-fatal: the
// slider falls back to procedural scenes.
type photoLibrary struct {
	cache []*image.RGBA
}

// photoCacheWidth is the cache raster width (≥2× the default 300px
// background) — big enough that hot-swapped sizes still downscale cleanly.
const photoCacheWidth = 640

func loadPhotoLibrary(dir string, bgW, bgH int) *photoLibrary {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("captcha backgrounds_dir unreadable, using procedural scenes",
			"dir", dir, "error", err)
		return nil
	}
	cacheH := photoCacheWidth * bgH / max(1, bgW)
	var cache []*image.RGBA
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if !strings.HasSuffix(name, ".jpg") && !strings.HasSuffix(name, ".jpeg") &&
			!strings.HasSuffix(name, ".png") {
			continue
		}
		img, err := loadImageFile(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("captcha background not decodable, skipped",
				"file", e.Name(), "error", err)
			continue
		}
		cache = append(cache, scaleCrop(img, photoCacheWidth, cacheH))
	}
	if len(cache) == 0 {
		slog.Warn("captcha backgrounds_dir has no usable images, using procedural scenes", "dir", dir)
		return nil
	}
	slog.Info("captcha photo backgrounds loaded", "dir", dir, "count", len(cache))
	return &photoLibrary{cache: cache}
}

// pick returns a random cached photo rescaled to the requested size.
func (l *photoLibrary) pick(rng *rand.Rand, w, h int) *image.RGBA {
	src := l.cache[int(rng.Float64()*float64(len(l.cache)))]
	if src.Bounds().Dx() == w && src.Bounds().Dy() == h {
		return src
	}
	return scaleCrop(src, w, h)
}

func loadImageFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

// scaleCrop centre-crops src to the target aspect ratio, then rescales with
// a box filter (area average) — dependency-free and plenty for backgrounds.
func scaleCrop(src image.Image, w, h int) *image.RGBA {
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 {
		return image.NewRGBA(image.Rect(0, 0, w, h))
	}
	// Crop to the target aspect first so scaling does not distort.
	cw, ch := sw, sh
	if sw*h > sh*w {
		cw = sh * w / h
	} else if sw*h < sh*w {
		ch = sw * h / w
	}
	ox, oy := b.Min.X+(sw-cw)/2, b.Min.Y+(sh-ch)/2

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy0 := oy + y*ch/h
		sy1 := oy + (y+1)*ch/h
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < w; x++ {
			sx0 := ox + x*cw/w
			sx1 := ox + (x+1)*cw/w
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, _ := src.At(sx, sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					bl += uint64(pb)
					n++
				}
			}
			// RGBA() is 16-bit premultiplied; opaque sources make the
			// premultiply a no-op.
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return dst
}
