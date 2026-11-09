package captcha

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"math"
	rand "math/rand/v2"
)

// newSceneRNG seeds a math/rand/v2 PCG generator from crypto/rand so every
// challenge scene is unpredictable.
func newSceneRNG() *rand.Rand {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("captcha: crypto/rand unavailable: %v", err))
	}
	return rand.New(rand.NewPCG(
		binary.LittleEndian.Uint64(b[:8]),
		binary.LittleEndian.Uint64(b[8:]),
	))
}

// Procedural background scenes for the slider challenge. Every Issue draws a
// fresh scene: a hand-tuned palette is picked and jittered, then composed from
// gradients, value noise and silhouettes. The final passes — vignette and 4×4
// Bayer ordered dithering — are what separate these from a naive gradient:
// dithering breaks up the banding that immediately reads as "generated".
// Pure stdlib; deterministic for a given rng state.

// rgb holds channel values in 0..255 as floats so intermediate math stays
// lossless; conversion back to uint8 clamps at the edges.
type rgb [3]float64

type colorStop struct {
	t float64
	c rgb
}

func mixRGB(a, b rgb, t float64) rgb {
	return rgb{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t}
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// evalGradient interpolates a multi-stop gradient at t∈[0,1].
func evalGradient(stops []colorStop, t float64) rgb {
	if t <= stops[0].t {
		return stops[0].c
	}
	last := stops[len(stops)-1]
	if t >= last.t {
		return last.c
	}
	for i := 1; i < len(stops); i++ {
		if t <= stops[i].t {
			span := stops[i].t - stops[i-1].t
			f := 0.0
			if span > 0 {
				f = (t - stops[i-1].t) / span
			}
			return mixRGB(stops[i-1].c, stops[i].c, f)
		}
	}
	return last.c
}

func fillGradientV(img *image.RGBA, stops []colorStop) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < h; y++ {
		c := evalGradient(stops, float64(y)/float64(h-1))
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), 255})
		}
	}
}

// fillGradientDiag fills along a diagonal: t = (x/w·kx + y/h·ky) normalized,
// kx+ky = 1. kx=0 is vertical, kx=1 horizontal.
func fillGradientDiag(img *image.RGBA, stops []colorStop, kx float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := (float64(x)/float64(w-1))*kx + (float64(y)/float64(h-1))*(1-kx)
			c := evalGradient(stops, t)
			img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), 255})
		}
	}
}

// hashLattice maps an integer lattice point to a stable pseudo-random [0,1)
// (integer-hash mixing; wrapping multiply is intentional).
func hashLattice(ix, iy int, seed uint32) float64 {
	h := uint32(ix)*0x9E3779B1 ^ uint32(iy)*0x85EBCA77 ^ seed*0xC2B2AE3D
	h ^= h >> 15
	h *= 0x27D4EB2F
	h ^= h >> 13
	return float64(h) / 4294967296.0
}

// valueNoise returns smooth noise in [0,1) at (x,y) with feature size ~scale.
func valueNoise(x, y, scale float64, seed uint32) float64 {
	if scale < 1 {
		scale = 1
	}
	x /= scale
	y /= scale
	ix, iy := math.Floor(x), math.Floor(y)
	fx, fy := x-ix, y-iy
	ux := fx * fx * (3 - 2*fx)
	uy := fy * fy * (3 - 2*fy)
	n00 := hashLattice(int(ix), int(iy), seed)
	n10 := hashLattice(int(ix)+1, int(iy), seed)
	n01 := hashLattice(int(ix), int(iy)+1, seed)
	n11 := hashLattice(int(ix)+1, int(iy)+1, seed)
	a := n00 + (n10-n00)*ux
	b := n01 + (n11-n01)*ux
	return a + (b-a)*uy
}

// fbm sums 2–3 octaves of valueNoise; the go-to for clouds, water and ridges.
func fbm(x, y, scale float64, octaves int, seed uint32) float64 {
	sum, amp, total, s := 0.0, 1.0, 0.0, scale
	for i := 0; i < octaves; i++ {
		sum += valueNoise(x, y, s, seed+uint32(i)*131) * amp
		total += amp
		amp *= 0.5
		s *= 0.5
	}
	return sum / total
}

// bayer4 holds the 4×4 Bayer matrix as additive offsets in [-0.5, 7/16].
var bayer4 = [4][4]float64{
	{-8.0 / 16, 0, -14.0 / 16, -6.0 / 16},
	{4.0 / 16, 12.0 / 16, -2.0 / 16, 10.0 / 16},
	{-12.0 / 16, -4.0 / 16, -15.0 / 16, -7.0 / 16},
	{7.0 / 16, 15.0 / 16, 3.0 / 16, 11.0 / 16},
}

// ditherPass adds ±strength (0..255 units) of ordered noise, trading the
// stair-step banding of smooth gradients for imperceptible texture.
func ditherPass(img *image.RGBA, strength float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			off := bayer4[y&3][x&3] * strength
			c := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(c.R) + off),
				clamp8(float64(c.G) + off),
				clamp8(float64(c.B) + off),
				c.A,
			})
		}
	}
}

// vignettePass darkens the corners by up to strength (0..1) with a quadratic
// falloff — a cheap photographic cue.
func vignettePass(img *image.RGBA, strength float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	cx, cy := float64(w-1)/2, float64(h-1)/2
	maxD := math.Hypot(cx, cy)
	if maxD == 0 {
		return
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy) / maxD
			f := 1 - strength*d*d
			c := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(c.R) * f),
				clamp8(float64(c.G) * f),
				clamp8(float64(c.B) * f),
				c.A,
			})
		}
	}
}

// addDisc paints a soft-edged disc (sun/moon) additively.
func addDisc(img *image.RGBA, cx, cy, r float64, c rgb) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	x0, x1 := int(cx-r-2), int(cx+r+2)
	y0, y1 := int(cy-r-2), int(cy+r+2)
	for y := max(0, y0); y <= min(h-1, y1); y++ {
		for x := max(0, x0); x <= min(w-1, x1); x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy)
			a := (r - d) / 2.5 // soft edge avoids a hard disc rim
			if a <= 0 {
				continue
			}
			if a > 1 {
				a = 1
			}
			p := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(p.R) + c[0]*a),
				clamp8(float64(p.G) + c[1]*a),
				clamp8(float64(p.B) + c[2]*a),
				255,
			})
		}
	}
}

// addGlow adds a wide quadratic-falloff halo (atmosphere around the sun).
func addGlow(img *image.RGBA, cx, cy, r float64, c rgb, intensity float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	x0, x1 := int(cx-r), int(cx+r)
	y0, y1 := int(cy-r), int(cy+r)
	for y := max(0, y0); y <= min(h-1, y1); y++ {
		for x := max(0, x0); x <= min(w-1, x1); x++ {
			d := math.Hypot(float64(x)-cx, float64(y)-cy) / r
			if d >= 1 {
				continue
			}
			a := intensity * (1 - d) * (1 - d)
			p := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(p.R) + c[0]*a),
				clamp8(float64(p.G) + c[1]*a),
				clamp8(float64(p.B) + c[2]*a),
				255,
			})
		}
	}
}

// blendPixel alpha-composites c (with alpha a∈[0,1]) onto (x,y).
func blendPixel(img *image.RGBA, x, y int, c rgb, a float64) {
	p := img.RGBAAt(x, y)
	img.SetRGBA(x, y, color.RGBA{
		clamp8(float64(p.R)*(1-a) + c[0]*a),
		clamp8(float64(p.G)*(1-a) + c[1]*a),
		clamp8(float64(p.B)*(1-a) + c[2]*a),
		255,
	})
}

// boxBlur returns a blurred copy via two separable sliding-window passes
// (O(pixels), independent of radius).
func boxBlur(src *image.RGBA, radius int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if radius < 1 || w < 2 || h < 2 {
		return src
	}
	tmp := image.NewRGBA(src.Bounds())
	dst := image.NewRGBA(src.Bounds())

	// horizontal: sliding sum over [x-r, x+r] with edge clamp
	for y := 0; y < h; y++ {
		var sr, sg, sb int
		for x := -radius; x <= radius; x++ {
			c := src.RGBAAt(clampI(x, 0, w-1), y)
			sr += int(c.R)
			sg += int(c.G)
			sb += int(c.B)
		}
		span := 2*radius + 1
		for x := 0; x < w; x++ {
			tmp.SetRGBA(x, y, color.RGBA{uint8(sr / span), uint8(sg / span), uint8(sb / span), 255})
			out := src.RGBAAt(clampI(x+radius+1, 0, w-1), y)
			in := src.RGBAAt(clampI(x-radius, 0, w-1), y)
			sr += int(out.R) - int(in.R)
			sg += int(out.G) - int(in.G)
			sb += int(out.B) - int(in.B)
		}
	}
	// vertical
	for x := 0; x < w; x++ {
		var sr, sg, sb int
		for y := -radius; y <= radius; y++ {
			c := tmp.RGBAAt(x, clampI(y, 0, h-1))
			sr += int(c.R)
			sg += int(c.G)
			sb += int(c.B)
		}
		span := 2*radius + 1
		for y := 0; y < h; y++ {
			dst.SetRGBA(x, y, color.RGBA{uint8(sr / span), uint8(sg / span), uint8(sb / span), 255})
			out := tmp.RGBAAt(x, clampI(y+radius+1, 0, h-1))
			in := tmp.RGBAAt(x, clampI(y-radius, 0, h-1))
			sr += int(out.R) - int(in.R)
			sg += int(out.G) - int(in.G)
			sb += int(out.B) - int(in.B)
		}
	}
	return dst
}

func clampI(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// jitter subtly shifts a palette so consecutive challenges never look
// identical while staying recognizably in-family.
func jitterRGB(c rgb, rng *rand.Rand, amt float64) rgb {
	j := func(v float64) float64 { return clampF(v+(rng.Float64()*2-1)*amt, 0, 255) }
	return rgb{j(c[0]), j(c[1]), j(c[2])}
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func pickPalette(rng *rand.Rand, n int) int { return int(rng.Float64() * float64(n)) }

// ---------------------------------------------------------------------------
// Scene registry
// ---------------------------------------------------------------------------

type sceneGen func(img *image.RGBA, rng *rand.Rand)

var sceneGens = []sceneGen{sceneDuskCity, sceneMountains, sceneOcean, sceneGeoMesh, sceneAurora}

// renderScene composes a fresh background. rng must be independently seeded
// per call (see newSceneRNG).
func renderScene(w, h int, rng *rand.Rand) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	gen := sceneGens[int(rng.Float64()*float64(len(sceneGens)))]
	gen(img, rng)
	vignettePass(img, 0.20+rng.Float64()*0.08)
	ditherPass(img, 3)
	return img
}

// ---------------------------------------------------------------------------
// 1. Dusk city skyline
// ---------------------------------------------------------------------------

var citySkies = [][]colorStop{
	{{0, rgb{30, 32, 74}}, {0.55, rgb{88, 52, 102}}, {0.85, rgb{222, 104, 90}}, {1, rgb{255, 178, 110}}},
	{{0, rgb{10, 27, 50}}, {0.5, rgb{24, 66, 96}}, {0.85, rgb{216, 120, 84}}, {1, rgb{255, 186, 120}}},
	{{0, rgb{22, 20, 56}}, {0.45, rgb{66, 44, 110}}, {0.8, rgb{178, 84, 130}}, {1, rgb{248, 160, 120}}},
	{{0, rgb{8, 16, 36}}, {0.6, rgb{28, 52, 88}}, {1, rgb{92, 120, 158}}}, // moonlit steel blue
}

func sceneDuskCity(img *image.RGBA, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	sky := citySkies[pickPalette(rng, len(citySkies))]
	fillGradientV(img, sky)

	horizonC := evalGradient(sky, 1)
	fw := float64(w)
	fh := float64(h)

	// Sun or moon low over the skyline.
	isMoon := sky[0].c[2] > 120 && horizonC[0] < 120
	cx := fw * (0.2 + rng.Float64()*0.6)
	cy := fh * (0.3 + rng.Float64()*0.18)
	if isMoon {
		addGlow(img, cx, cy, fh*0.55, rgb{60, 80, 120}, 0.35)
		addDisc(img, cx, cy, fh*0.07, rgb{120, 140, 170})
	} else {
		addGlow(img, cx, cy, fh*0.7, rgb{255, 150, 80}, 0.5)
		addDisc(img, cx, cy, fh*0.08, rgb{255, 200, 130})
	}

	// Building layers, back to front: farther layers sit higher, are lighter
	// (atmospheric haze) and lack windows.
	night := rgb{10, 12, 24}
	for layer := 0; layer < 3; layer++ {
		t := 0.45 + 0.22*float64(layer)
		col := mixRGB(horizonC, night, t)
		topBase := fh * (0.52 - 0.04*float64(layer))
		topVar := fh * (0.10 + 0.10*float64(layer))
		x := -float64(rng.Float64()) * 10
		minBW, maxBW := 9.0+4*float64(layer), 26.0+6*float64(layer)
		for x < fw {
			bw := minBW + rng.Float64()*(maxBW-minBW)
			top := topBase + rng.Float64()*topVar
			bCol := jitterRGB(col, rng, 4)
			bxi, byi := int(x), int(top)
			bxj, byj := min(w-1, int(x+bw)), h-1
			for y := byi; y <= byj; y++ {
				for px := bxi; px <= bxj; px++ {
					if px < 0 {
						continue
					}
					img.SetRGBA(px, y, color.RGBA{clamp8(bCol[0]), clamp8(bCol[1]), clamp8(bCol[2]), 255})
				}
			}
			// Antenna spikes on some front-layer towers.
			if layer == 2 && rng.Float64() < 0.3 && bw > 14 {
				ax := bxi + int(bw/2)
				for y := byi - int(4+rng.Float64()*6); y < byi; y++ {
					if y >= 0 && ax >= 0 && ax < w {
						img.SetRGBA(ax, y, color.RGBA{clamp8(bCol[0]), clamp8(bCol[1]), clamp8(bCol[2]), 255})
					}
				}
			}
			// Rooftop boxes (water tanks / lift rooms) break the flat
			// roofline so it does not read as stacked rectangles.
			if layer == 2 && rng.Float64() < 0.55 && bw > 15 {
				rw := 3 + rng.Float64()*4
				rh := 2 + rng.Float64()*3
				rx := float64(bxi) + 2 + rng.Float64()*(bw-4-rw)
				for y := int(top - rh); y < byi; y++ {
					for px := int(rx); px < int(rx+rw); px++ {
						if y >= 0 && px >= 0 && px < w {
							img.SetRGBA(px, y, color.RGBA{clamp8(bCol[0]), clamp8(bCol[1]), clamp8(bCol[2]), 255})
						}
					}
				}
			}
			// Lit windows only on the front layer. Whole floors go dark at
			// random and column steps jitter per building — a uniform grid of
			// dots is the most obvious "generated" tell.
			if layer == 2 {
				colStep := 3 + int(rng.Float64()*3)
				for wy := byi + 3; wy < byj-2; wy += 5 {
					floorLit := rng.Float64() < 0.8
					wxOff := int(rng.Float64() * 3)
					for wx := bxi + 2 + wxOff; wx < bxj-2; wx += colStep {
						if floorLit && rng.Float64() < 0.3 {
							wc := rgb{255, 214, 140}
							if rng.Float64() < 0.2 {
								wc = rgb{180, 220, 255}
							}
							blendPixel(img, wx, wy, wc, 0.55+rng.Float64()*0.35)
							if wy+1 <= byj && wx < bxj-2 {
								blendPixel(img, wx, wy+1, wc, 0.5)
							}
						}
					}
				}
			}
			x += bw + 1
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Mountain ridges
// ---------------------------------------------------------------------------

var mountainSkies = [][]colorStop{
	{{0, rgb{46, 58, 102}}, {0.5, rgb{150, 116, 146}}, {0.8, rgb{248, 186, 142}}, {1, rgb{255, 214, 160}}},
	{{0, rgb{88, 140, 200}}, {0.55, rgb{150, 190, 226}}, {1, rgb{210, 228, 240}}},
	{{0, rgb{24, 30, 66}}, {0.5, rgb{86, 64, 120}}, {0.85, rgb{230, 130, 100}}, {1, rgb{255, 180, 120}}},
	{{0, rgb{30, 40, 80}}, {0.6, rgb{120, 110, 160}}, {1, rgb{240, 200, 170}}},
}

func sceneMountains(img *image.RGBA, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	sky := mountainSkies[pickPalette(rng, len(mountainSkies))]
	fillGradientV(img, sky)
	fw, fh := float64(w), float64(h)
	horizonC := evalGradient(sky, 1)

	cx := fw * (0.15 + rng.Float64()*0.7)
	cy := fh * (0.18 + rng.Float64()*0.16)
	addGlow(img, cx, cy, fh*0.75, rgb{255, 170, 110}, 0.4)
	addDisc(img, cx, cy, fh*0.07, rgb{255, 225, 170})

	// Horizon haze: a light band where sky meets the far ridge softens the
	// transition and adds a third depth plane.
	hazeC := evalGradient(sky, 0.92)
	for y := int(fh * 0.40); y < int(fh*0.52); y++ {
		for x := 0; x < w; x++ {
			blendPixel(img, x, y, hazeC, 0.25)
		}
	}

	// Ridge layers back→front: tops creep down the frame, contrast rises
	// (atmospheric perspective). Far layers keep 3 fbm octaves (craggy);
	// near layers use 2 + a peak envelope so they read as mountain masses,
	// not uniform sawtooth.
	ridgeDark := rgb{26, 30, 44}
	for layer := 0; layer < 4; layer++ {
		t := 0.30 + 0.19*float64(layer)
		col := mixRGB(horizonC, ridgeDark, t)
		baseY := fh * (0.42 + 0.115*float64(layer))
		amp := fh * (0.05 + 0.032*float64(layer))
		seed := uint32(1000 + layer*77 + int(rng.Float64()*9000))
		freq := fw / (55 + 25*rng.Float64())
		octaves := 3
		if layer >= 2 {
			octaves = 2
		}
		for x := 0; x < w; x++ {
			// 1-D fbm along x with a slow domain warp: warping the sample
			// point breaks the even wave rhythm that pure fbm produces.
			warp := (valueNoise(float64(x)/46, float64(layer)*7.7, 1, seed+5) - 0.5) * fw * 0.05
			envelope := 0.6 + 0.8*valueNoise(float64(x)/90, float64(layer)*3.3, 1, seed+9)
			n := fbm(float64(x)+warp, float64(layer)*13.7, freq, octaves, seed)
			top := baseY - amp*(n*1.6-0.3)*envelope
			for y := clampI(int(top), 0, h-1); y < h; y++ {
				depthFade := (float64(y) - top) / fh
				c := mixRGB(col, ridgeDark, 0.15+0.5*clampF(depthFade, 0, 1))
				img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), 255})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 3. Ocean sunset
// ---------------------------------------------------------------------------

var oceanSkies = [][]colorStop{
	{{0, rgb{26, 32, 72}}, {0.45, rgb{120, 70, 110}}, {0.8, rgb{244, 140, 96}}, {1, rgb{255, 200, 130}}},
	{{0, rgb{40, 44, 92}}, {0.5, rgb{170, 110, 90}}, {0.85, rgb{250, 190, 120}}, {1, rgb{255, 220, 160}}},
	{{0, rgb{30, 44, 70}}, {0.5, rgb{70, 100, 130}}, {1, rgb{160, 190, 205}}},
	{{0, rgb{16, 24, 56}}, {0.5, rgb{60, 60, 110}}, {0.85, rgb{210, 120, 110}}, {1, rgb{250, 170, 120}}},
}

func sceneOcean(img *image.RGBA, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	sky := oceanSkies[pickPalette(rng, len(oceanSkies))]
	fillGradientV(img, sky)
	fw, fh := float64(w), float64(h)
	horizon := int(fh * (0.5 + rng.Float64()*0.1))
	seaTop := evalGradient(sky, 1)

	// Sea: reflect the sky, shifted cooler and darker with depth.
	for y := horizon; y < h; y++ {
		t := float64(y-horizon) / float64(h-horizon)
		c := mixRGB(mixRGB(seaTop, rgb{20, 40, 60}, 0.45), rgb{8, 20, 36}, t*0.55)
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), 255})
		}
	}

	// Half-set sun on the horizon.
	cx := fw * (0.25 + rng.Float64()*0.5)
	sunR := fh * 0.075
	addGlow(img, cx, float64(horizon)-sunR*0.3, fh*0.85, rgb{255, 160, 90}, 0.45)
	addDisc(img, cx, float64(horizon)-sunR*0.35, sunR, rgb{255, 205, 140})

	// Shimmering light path under the sun, widening with distance. Two noise
	// octaves + a per-row spread jitter keep the streaks from settling into
	// regular rows of dashes.
	glowC := mixRGB(seaTop, rgb{255, 210, 140}, 0.6)
	seed := uint32(int(rng.Float64() * 100000))
	for y := horizon + 1; y < h; y++ {
		t := float64(y-horizon) / float64(h-horizon)
		spread := (3 + t*fw*0.09) * (0.7 + 0.6*valueNoise(float64(y)/3, 0.5, 1, seed+3))
		for x := 0; x < w; x++ {
			dx := math.Abs(float64(x) - cx)
			if dx > spread {
				continue
			}
			streak := 0.6*valueNoise(float64(x)/9, float64(y)/2.2, 1, seed) +
				0.4*valueNoise(float64(x)/26, float64(y)/6, 1, seed+17)
			intensity := math.Pow(1-dx/spread, 1.6) * (0.30 + 0.70*streak) * (1 - t*0.45)
			p := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(p.R) + glowC[0]*intensity*0.55),
				clamp8(float64(p.G) + glowC[1]*intensity*0.55),
				clamp8(float64(p.B) + glowC[2]*intensity*0.55),
				255,
			})
		}
	}

	// Sparse darker wave strokes for texture.
	for y := horizon + 2; y < h; y += 2 {
		for x := 0; x < w; x++ {
			n := valueNoise(float64(x)/14, float64(y)/3.5, 1, seed+7)
			if n > 0.72 {
				blendPixel(img, x, y, rgb{6, 14, 28}, 0.35)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Geometric / mesh-gradient abstract
// ---------------------------------------------------------------------------

type geoPalette struct {
	base  []colorStop // diagonal base gradient
	blobs []rgb       // glow blob colors
	light bool        // light theme → screen blend instead of add
}

var geoPalettes = []geoPalette{
	{[]colorStop{{0, rgb{13, 18, 38}}, {1, rgb{26, 36, 70}}}, []rgb{{64, 120, 255}, {34, 220, 200}, {255, 255, 255}}, false},
	{[]colorStop{{0, rgb{18, 12, 36}}, {1, rgb{42, 28, 82}}}, []rgb{{168, 85, 255}, {56, 190, 255}}, false},
	{[]colorStop{{0, rgb{20, 26, 34}}, {1, rgb{34, 48, 58}}}, []rgb{{20, 184, 166}, {250, 204, 21}}, false},
	{[]colorStop{{0, rgb{242, 245, 252}}, {1, rgb{214, 225, 244}}}, []rgb{{96, 145, 255}, {130, 205, 255}, {255, 170, 140}}, true},
	{[]colorStop{{0, rgb{250, 246, 240}}, {1, rgb{228, 220, 210}}}, []rgb{{255, 150, 108}, {255, 198, 138}, {186, 158, 255}}, true},
}

func sceneGeoMesh(img *image.RGBA, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	pal := geoPalettes[pickPalette(rng, len(geoPalettes))]
	fillGradientDiag(img, pal.base, 0.35+rng.Float64()*0.3)
	fw, fh := float64(w), float64(h)

	// Glow blobs rendered on their own layer, heavily blurred, then blended —
	// the "mesh gradient" look without any color-space magic. One focal blob
	// gets extra punch so the composition has a clear light centre; decorative
	// accent lines were tried and read as scratches, so depth comes from the
	// blobs alone.
	layer := image.NewRGBA(img.Bounds())
	nBlob := 4 + int(rng.Float64()*2)
	focal := int(rng.Float64() * float64(nBlob))
	for i := 0; i < nBlob; i++ {
		c := pal.blobs[i%len(pal.blobs)]
		boost := 0.8
		if i == focal {
			boost = 1.15
		}
		blob := image.NewRGBA(img.Bounds())
		cx := fw * rng.Float64()
		cy := fh * rng.Float64()
		r := (0.26 + rng.Float64()*0.32) * math.Min(fw, fh)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				d := math.Hypot(float64(x)-cx, float64(y)-cy) / r
				if d < 1 {
					a := (1 - d) * (1 - d) * 255 * boost
					blob.SetRGBA(x, y, color.RGBA{clamp8(c[0] * boost), clamp8(c[1] * boost), clamp8(c[2] * boost), clamp8(a)})
				}
			}
		}
		blob = boxBlur(blob, max(2, int(math.Min(fw, fh)/6)))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				b := blob.RGBAAt(x, y)
				if b.A == 0 {
					continue
				}
				l := layer.RGBAAt(x, y)
				layer.SetRGBA(x, y, color.RGBA{
					clamp8(float64(l.R) + float64(b.R)*(float64(b.A)/255)),
					clamp8(float64(l.G) + float64(b.G)*(float64(b.A)/255)),
					clamp8(float64(l.B) + float64(b.B)*(float64(b.A)/255)),
					255,
				})
			}
		}
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			l := layer.RGBAAt(x, y)
			p := img.RGBAAt(x, y)
			if pal.light {
				// Normal alpha-over: screen on a near-white base washes out to
				// nothing, so light themes tint via plain compositing.
				a := clampF(float64(max(l.R, max(l.G, l.B)))/255*0.8, 0, 0.85)
				img.SetRGBA(x, y, color.RGBA{
					clamp8(float64(p.R)*(1-a) + float64(l.R)*a),
					clamp8(float64(p.G)*(1-a) + float64(l.G)*a),
					clamp8(float64(p.B)*(1-a) + float64(l.B)*a),
					255,
				})
			} else {
				img.SetRGBA(x, y, color.RGBA{
					clamp8(float64(p.R)*0.8 + float64(l.R)*0.95),
					clamp8(float64(p.G)*0.8 + float64(l.G)*0.95),
					clamp8(float64(p.B)*0.8 + float64(l.B)*0.95),
					255,
				})
			}
		}
	}

	// Light variants get a fine dot lattice for structure — without it they
	// read as a blank wash and the gap loses contrast against them.
	if pal.light {
		dot := mixRGB(pal.blobs[0], rgb{70, 80, 100}, 0.45)
		for y := 7; y < h; y += 13 {
			for x := 7 + ((y/13)%2)*6; x < w; x += 13 {
				blendPixel(img, x, y, dot, 0.16)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Aurora night sky
// ---------------------------------------------------------------------------

var auroraBandSets = [][]rgb{
	{{64, 255, 170}, {80, 200, 255}},
	{{150, 110, 255}, {70, 230, 200}},
	{{255, 130, 200}, {130, 170, 255}},
}

func sceneAurora(img *image.RGBA, rng *rand.Rand) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	fillGradientV(img, []colorStop{{0, rgb{5, 9, 26}}, {0.6, rgb{11, 24, 50}}, {1, rgb{18, 42, 68}}})
	fw, fh := float64(w), float64(h)

	// Stars: dense small points, a few bright ones with a soft halo.
	nStars := w * h / 260
	for i := 0; i < nStars; i++ {
		x := int(rng.Float64() * fw)
		y := int(rng.Float64() * fh * 0.72)
		b := 0.25 + rng.Float64()*0.75
		c := rgb{220 * b, 232 * b, 255 * b}
		img.SetRGBA(x, y, color.RGBA{clamp8(c[0]), clamp8(c[1]), clamp8(c[2]), 255})
		if rng.Float64() < 0.06 {
			addGlow(img, float64(x), float64(y), 3.5, rgb{200, 220, 255}, 0.5)
		}
	}

	// Aurora bands on their own layer, blurred, then added — curtains of
	// light whose bottom edge glows and fades upward.
	bands := auroraBandSets[pickPalette(rng, len(auroraBandSets))]
	layer := image.NewRGBA(img.Bounds())
	nBands := 2 + int(rng.Float64()*2)
	for i := 0; i < nBands; i++ {
		c1, c2 := jitterRGB(bands[0], rng, 18), jitterRGB(bands[1], rng, 18)
		if rng.Float64() < 0.5 {
			c1, c2 = c2, c1
		}
		y0 := fh * (0.14 + 0.16*float64(i) + rng.Float64()*0.06)
		th := fh * (0.14 + rng.Float64()*0.08)
		ph1 := rng.Float64() * math.Pi * 2
		ph2 := rng.Float64() * math.Pi * 2
		amp1, amp2 := fh*(0.04+rng.Float64()*0.05), fh*(0.02+rng.Float64()*0.04)
		seedB := uint32(int(rng.Float64() * 1e6))
		for x := 0; x < w; x++ {
			fx := float64(x)
			// Two sines + low-frequency noise drift break the uniform wave
			// rhythm; a fixed-y noise term gives column-wise rays — the
			// curtain-of-light signature of real aurorae.
			drift := (valueNoise(fx/26, float64(i)*3.7, 1, seedB+11)-0.5)*fh*0.14 +
				(valueNoise(fx/64, float64(i)*5.1, 1, seedB+41)-0.5)*fh*0.10
			ray := 0.35 + 0.65*valueNoise(fx/5.5, float64(i)*7.3, 1, seedB+29)
			path := y0 + amp1*math.Sin(fx*0.022+ph1) + amp2*math.Sin(fx*0.045+ph2) + drift
			edge := math.Sin(math.Pi * fx / fw) // fade at frame edges
			for y := 0; y < h; y++ {
				fy := float64(y)
				if fy > path || fy < path-th {
					continue
				}
				ty := (path - fy) / th // 0 bottom edge → 1 top
				intensity := math.Pow(1-ty, 1.7) * math.Pow(clampF(edge, 0, 1), 0.6) * ray
				c := mixRGB(c1, c2, ty)
				p := layer.RGBAAt(x, y)
				layer.SetRGBA(x, y, color.RGBA{
					clamp8(float64(p.R) + c[0]*intensity),
					clamp8(float64(p.G) + c[1]*intensity),
					clamp8(float64(p.B) + c[2]*intensity),
					255,
				})
			}
		}
	}
	layer = boxBlur(layer, max(2, int(fh/22)))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			l := layer.RGBAAt(x, y)
			if l.R|l.G|l.B == 0 {
				continue
			}
			p := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{
				clamp8(float64(p.R) + float64(l.R)*0.85),
				clamp8(float64(p.G) + float64(l.G)*0.85),
				clamp8(float64(p.B) + float64(l.B)*0.85),
				255,
			})
		}
	}

	// Dark ridge silhouette anchoring the bottom. A slow envelope makes a few
	// dominant peaks instead of uniform jaggedness.
	seed := uint32(int(rng.Float64() * 100000))
	baseY := fh * 0.82
	ridgeScale := fw / (40 + 20*rng.Float64())
	for x := 0; x < w; x++ {
		fx := float64(x)
		n := fbm(fx, 3.3, ridgeScale, 3, seed)
		envelope := 0.55 + 0.9*valueNoise(fx/95, 5.5, 1, seed+9)
		top := baseY - fh*0.075*(n*1.7-0.35)*envelope
		for y := clampI(int(top), 0, h-1); y < h; y++ {
			img.SetRGBA(x, y, color.RGBA{8, 15, 28, 255})
		}
	}
}
