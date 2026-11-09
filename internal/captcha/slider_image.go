package captcha

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

// Jigsaw rendering. The piece lives inside an N×N canvas (same box the client
// positions with translate(x, gapY)), shaped as a rounded square inset by the
// tab radius with semicircular tabs on the top and left edges — the classic
// puzzle silhouette. Anti-aliasing comes from 2×2 analytic supersampling of
// the shape; the white stroke is the ring between the shape and itself grown
// by the stroke width. Geometry here is decorative; anti-bot defense lives in
// the trajectory check (see the design doc).

// jigsawInset returns the tab radius / body inset for a piece of side n.
func jigsawInset(n int) float64 {
	inset := float64(n) / 7.0
	if inset < 3 {
		inset = 3
	}
	return inset
}

// insideJigsaw reports whether (x,y) (in the N×N piece canvas) falls inside
// the puzzle shape: rounded-square body plus top and left semicircular tabs.
func insideJigsaw(x, y, n, inset, cornerR, tabR float64) bool {
	lo, hi := inset, n-inset
	// Body: rounded rect [lo,hi]² with corner radius cornerR.
	if x >= lo && x <= hi && y >= lo && y <= hi {
		dx := math.Max(lo+cornerR-x, x-(hi-cornerR))
		dy := math.Max(lo+cornerR-y, y-(hi-cornerR))
		if (dx <= 0 || dy <= 0) || dx*dx+dy*dy <= cornerR*cornerR {
			return true
		}
	}
	// Top tab: circle centred on the body's top edge.
	if math.Hypot(x-n/2, y-lo) <= tabR {
		return true
	}
	// Left tab: circle centred on the body's left edge.
	return math.Hypot(x-lo, y-n/2) <= tabR
}

// shapeCoverage samples the shape 2×2 per pixel → AA alpha in [0,1].
func shapeCoverage(px, py int, n, inset, cornerR, tabR float64) float64 {
	const step = 0.25
	inside := 0
	for _, oy := range [2]float64{step, 0.75} {
		for _, ox := range [2]float64{step, 0.75} {
			if insideJigsaw(float64(px)+ox, float64(py)+oy, n, inset, cornerR, tabR) {
				inside++
			}
		}
	}
	return float64(inside) / 4
}

// strokeWidth is the white outline thickness (px) on piece and gap.
const strokeWidth = 2.0

// renderSlider composes the challenge images from a scene: the background
// with a translucent white gap + stroke (JPEG), and the jigsaw piece sliced
// from the same scene with a matching stroke (PNG, alpha-masked).
func renderSlider(scene *image.RGBA, cfg SliderConfig, gapX, gapY int) (bg, piece string) {
	n := float64(cfg.PieceSize)
	inset := jigsawInset(cfg.PieceSize)
	cornerR := inset * 0.65

	bgImg := image.NewRGBA(image.Rect(0, 0, cfg.BGWidth, cfg.BGHeight))
	draw.Draw(bgImg, bgImg.Bounds(), scene, image.Point{}, draw.Src)
	drawGap(bgImg, cfg.PieceSize, gapX, gapY, n, inset, cornerR)

	pieceImg := renderPiece(scene, cfg.PieceSize, gapX, gapY, n, inset, cornerR)

	return encodeJPEG(bgImg, 92), encodePNG(pieceImg)
}

// drawGap marks the target position on the background: the shape filled with
// translucent white plus an opaque white stroke around it.
func drawGap(img *image.RGBA, size, gapX, gapY int, n, inset, cornerR float64) {
	hi := gapX + size
	for y := gapY; y < hi; y++ {
		if y < 0 || y >= img.Bounds().Dy() {
			continue
		}
		for x := gapX; x < hi; x++ {
			if x < 0 || x >= img.Bounds().Dx() {
				continue
			}
			content := shapeCoverage(x-gapX, y-gapY, n, inset, cornerR, inset)
			stroke := shapeCoverage(x-gapX, y-gapY, n, inset-strokeWidth, cornerR+strokeWidth, inset+strokeWidth)
			if stroke <= 0 {
				continue
			}
			ring := clampF(stroke-content, 0, 1)
			fill := clampF(content, 0, 1)
			// Stroke ring nearly opaque; interior veil lets the scene show
			// through so the gap stays identifiable on any background.
			a := ring*0.92 + fill*0.30*(1-ring)
			blendPixel(img, x, y, rgb{255, 255, 255}, a)
		}
	}
}

// renderPiece slices the jigsaw piece out of the scene at (gapX,gapY) with a
// white stroke following the silhouette.
func renderPiece(scene *image.RGBA, size, gapX, gapY int, n, inset, cornerR float64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			content := shapeCoverage(x, y, n, inset, cornerR, inset)
			stroke := shapeCoverage(x, y, n, inset-strokeWidth, cornerR+strokeWidth, inset+strokeWidth)
			if stroke <= 0 {
				continue // fully outside piece + stroke: transparent
			}
			ring := clampF(stroke-content, 0, 1)
			total := content + ring*(1-content)
			if total <= 0 {
				continue
			}
			sx, sy := gapX+x, gapY+y
			var sc rgb
			if sx >= 0 && sx < scene.Bounds().Dx() && sy >= 0 && sy < scene.Bounds().Dy() {
				p := scene.RGBAAt(sx, sy)
				sc = rgb{float64(p.R), float64(p.G), float64(p.B)}
			}
			// White ring shows through where content coverage drops off.
			c := rgb{
				(sc[0]*content*(1-ring) + 255*ring) / total,
				(sc[1]*content*(1-ring) + 255*ring) / total,
				(sc[2]*content*(1-ring) + 255*ring) / total,
			}
			img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), clamp8(total * 255)})
		}
	}
	return img
}

func encodePNG(img image.Image) string {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func encodeJPEG(img image.Image, quality int) string {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
