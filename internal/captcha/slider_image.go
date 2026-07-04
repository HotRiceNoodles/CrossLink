package captcha

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// renderSlider generates the background (textured, with a shadowed gap at
// (gapX,gapY)) and the puzzle piece (the un-shadowed texture fragment that the
// client drags into the gap). Returns both as base64 PNG strings.
//
// Plain stdlib only — no external image dependency. The gap is a square; a
// nicer jigsaw-tab shape can be layered on later without changing the
// Provider contract. Geometry here is decorative; anti-bot defense lives in
// the trajectory check (see section 5 of the design doc).
func renderSlider(cfg SliderConfig, gapX, gapY int) (bg, piece string) {
	tex := generateTexture(cfg.BGWidth, cfg.BGHeight, gapX, gapY)

	bgImg := image.NewRGBA(tex.Bounds())
	draw.Draw(bgImg, bgImg.Bounds(), tex, image.Point{}, draw.Src)
	drawGapShadow(bgImg, gapX, gapY, cfg.PieceSize)

	pieceImg := image.NewRGBA(image.Rect(0, 0, cfg.PieceSize, cfg.PieceSize))
	draw.Draw(pieceImg, pieceImg.Bounds(), tex, image.Pt(gapX, gapY), draw.Src)

	return encodePNG(bgImg), encodePNG(pieceImg)
}

// generateTexture builds a non-uniform colored background so the gap (and the
// matching puzzle piece) are visually identifiable. Colors vary smoothly via
// layered sines seeded by the gap position, with a fine noise term.
func generateTexture(w, h, gapX, gapY int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seedX := float64(gapX) * 0.07
	seedY := float64(gapY) * 0.09
	for y := 0; y < h; y++ {
		fy := float64(y)
		for x := 0; x < w; x++ {
			fx := float64(x)
			// three sine bands → smooth color variation
			r := 0.5 + 0.5*math.Sin(fx*0.05+seedX)
			g := 0.5 + 0.5*math.Sin(fy*0.04+seedY+2.0)
			b := 0.5 + 0.5*math.Sin((fx+fy)*0.03+seedX+seedY)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(r * 200),
				G: uint8(g * 200),
				B: uint8(b * 200),
				A: 255,
			})
		}
	}
	return img
}

// drawGapShadow darkens the gap region on the background so the user can see
// where to drag the piece. A thin outline marks the border.
func drawGapShadow(img *image.RGBA, gapX, gapY, size int) {
	overlay := color.RGBA{R: 0, G: 0, B: 0, A: 90}
	for dy := 0; dy < size; dy++ {
		for dx := 0; dx < size; dx++ {
			c := overlay
			if dx == 0 || dy == 0 || dx == size-1 || dy == size-1 {
				c = color.RGBA{R: 255, G: 255, B: 255, A: 200} // border
			}
			blend(img, gapX+dx, gapY+dy, c)
		}
	}
}

// blend alpha-composites c onto the pixel at (x,y).
func blend(img *image.RGBA, x, y int, c color.RGBA) {
	if !image.Pt(x, y).In(img.Bounds()) {
		return
	}
	srcA := float64(c.A) / 255.0
	dst := img.RGBAAt(x, y)
	mix := func(s, d uint8) uint8 {
		return uint8(float64(s)*srcA + float64(d)*(1-srcA))
	}
	img.SetRGBA(x, y, color.RGBA{
		R: mix(c.R, dst.R),
		G: mix(c.G, dst.G),
		B: mix(c.B, dst.B),
		A: 255,
	})
}

func encodePNG(img image.Image) string {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
