package geo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	xdraw "golang.org/x/image/draw"
)

// The test photos are drawn, not downloaded: a street scene of sky,
// buildings with windows and a textured road, and variations on it that
// each fail one check.

// scene is the street scene, w × h, drawn with r's noise.
func scene(w, h int, r *rand.Rand) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	horizon := h / 3
	for y := range h {
		for x := range w {
			var c color.RGBA
			switch {
			case y < horizon: // sky, paler towards the horizon
				k := uint8(80 * y / horizon)
				c = color.RGBA{90 + k, 140 + k, 220, 255}
			case y < h*3/4: // buildings, each its own colour, with windows
				b := x * 7 / w
				c = color.RGBA{uint8(60 + 25*b), uint8(50 + 15*b), uint8(40 + 10*b), 255}
				if (x*40/w)%3 == 1 && ((y-horizon)*20/h)%2 == 1 {
					c = color.RGBA{30, 35, 45, 255}
				}
			default: // road with lane marks
				c = color.RGBA{105, 105, 110, 255}
				if (x*10/w)%2 == 0 && y > h*7/8 && y < h*7/8+h/40 {
					c = color.RGBA{230, 230, 230, 255}
				}
			}
			n := int(r.NormFloat64() * 10)
			m.SetRGBA(x, y, color.RGBA{clamp(int(c.R) + n), clamp(int(c.G) + n), clamp(int(c.B) + n), 255})
		}
	}
	// A tall building cuts into the sky.
	for y := h / 6; y < horizon; y++ {
		for x := w / 3; x < w/2; x++ {
			m.SetRGBA(x, y, color.RGBA{150, 120, 100, 255})
		}
	}
	return m
}

func clamp(v int) uint8 { return uint8(max(0, min(255, v))) }

// mapPixels sets every pixel of m by f of its position and colour.
func mapPixels(m *image.RGBA, f func(x, y int, c color.RGBA) color.RGBA) *image.RGBA {
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			m.SetRGBA(x, y, f(x, y, m.RGBAAt(x, y)))
		}
	}
	return m
}

// blur is m out of focus: shrunk by k and scaled back up.
func blur(m *image.RGBA, k int) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	return scale(scale(m, w/k, h/k, xdraw.ApproxBiLinear), w, h, xdraw.BiLinear)
}

func jpegOf(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testPhotos are the test photos at w × h: name, image, the Reject wanted.
func testPhotos(w, h int) []struct {
	name string
	m    *image.RGBA
	want error
} {
	r := rand.New(rand.NewPCG(1, 2))
	return []struct {
		name string
		m    *image.RGBA
		want error
	}{
		{"good", scene(w, h, r), nil},
		{"night", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{c.R / 6, c.G / 6, c.B / 6, 255}
		}), RejectExposure},
		{"burnt out", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{clamp(int(c.R) * 3), clamp(int(c.G) * 3), clamp(int(c.B) * 3), 255}
		}), RejectExposure},
		{"fog", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{150 + c.R/10, 150 + c.G/10, 150 + c.B/10, 255}
		}), RejectContrast},
		{"blurry", blur(scene(w, h, r), max(w/64, 4)), RejectBlurry},
		{"sky", mapPixels(scene(w, h, r), func(x, y int, c color.RGBA) color.RGBA {
			if y < h*2/3 {
				return color.RGBA{170, 200, 240, 255}
			}
			return c
		}), RejectSky},
		{"dashboard", mapPixels(scene(w, h, r), func(_, y int, c color.RGBA) color.RGBA {
			if y >= h*65/100 {
				return color.RGBA{20, 20, 22, 255}
			}
			return c
		}), RejectDashboard},
		{"bare wall", bareWall(w, h, r), RejectBare},
	}
}

// bareWall is a smooth, sharp-grained plaster wall: exposed well, with
// contrast and grain, but no edges.
func bareWall(w, h int, r *rand.Rand) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			l := 60 + 140*float64(x)/float64(w)
			n := r.NormFloat64() * 6 * float64(w) / analysisWidth
			m.SetRGBA(x, y, color.RGBA{clamp(int(l + n + 20)), clamp(int(l + n)), clamp(int(l + n - 25)), 255})
		}
	}
	return m
}

func TestCheckPixels(t *testing.T) {
	for _, tc := range testPhotos(analysisWidth, analysisWidth*3/4) {
		_, err := checkPixels(tc.m)
		if err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestVet(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(3, 4))
	for _, tc := range testPhotos(1024, 768) {
		out, err := Vet(jpegOf(t, tc.m), false, r)
		if err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
			continue
		}
		if err != nil {
			continue
		}
		m, format, err := image.Decode(bytes.NewReader(out))
		if err != nil || format != "jpeg" {
			t.Fatalf("%s: output %s, %v", tc.name, format, err)
		}
		if m.Bounds().Dx() != 1024 || m.Bounds().Dy() != 768 || len(out) > maxOut {
			t.Errorf("%s: output %v, %d bytes", tc.name, m.Bounds(), len(out))
		}
	}
}

func TestVetScalesAndCrops(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(5, 6))
	// A wide photo with a short dashboard band: shrunk to 1600 wide, and
	// the band cut off.
	m := mapPixels(scene(2048, 1536, r), func(_, y int, c color.RGBA) color.RGBA {
		if y >= 1536*90/100 {
			return color.RGBA{15, 15, 18, 255}
		}
		return c
	})
	out, err := Vet(jpegOf(t, m), false, r)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 1600 || cfg.Height > 1200*91/100 || cfg.Height < 1200*85/100 {
		t.Errorf("output %d × %d, want 1600 wide with about the bottom 10%% cut", cfg.Width, cfg.Height)
	}
}

func TestVetRejects(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"not an image", []byte("hello"), RejectUnreadable},
		{"truncated", jpegOf(t, scene(1280, 960, r))[:500], RejectUnreadable},
		{"narrow", jpegOf(t, scene(800, 600, r)), RejectSmall},
	} {
		if _, err := Vet(tc.data, false, r); err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// equirect is a panorama that is a street scene ahead (its centre, the
// capture heading) and blank grey all round the rest.
func equirect(w, h int, r *rand.Rand) *image.RGBA {
	pano := mapPixels(image.NewRGBA(image.Rect(0, 0, w, h)), func(_, _ int, _ color.RGBA) color.RGBA {
		return color.RGBA{128, 128, 128, 255}
	})
	front := scene(w/2, h, r)
	for y := range h {
		for x := range w / 2 {
			pano.SetRGBA(w/4+x, y, front.RGBAAt(x, y))
		}
	}
	return pano
}

func TestVetPanorama(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(9, 10))
	data := jpegOf(t, equirect(2048, 1024, r))
	out, err := Vet(data, true, r)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 1024 || cfg.Height != 768 {
		t.Errorf("view %d × %d, want 1024 × 768", cfg.Width, cfg.Height)
	}
	// A photo that isn't 2:1 is checked as a flat photo.
	if _, err := Vet(jpegOf(t, scene(1024, 768, r)), true, r); err != nil {
		t.Errorf("a pano-flagged 4:3 photo: %v", err)
	}
}

func TestGnomonic(t *testing.T) {
	// A panorama whose colour is its longitude: the view's centre column
	// shows the yaw, and its edges 45° either side.
	src := image.NewRGBA(image.Rect(0, 0, 360, 180))
	mapPixels(src, func(x, _ int, _ color.RGBA) color.RGBA { return color.RGBA{uint8(x * 255 / 359), 0, 0, 255} })
	for _, yaw := range []float64{0, math.Pi / 4, -math.Pi / 3} {
		v := gnomonic(src, yaw, 101, 75)
		lonAt := func(x int) float64 { return float64(v.RGBAAt(x, 37).R)*359/255 - 180 }
		want := yaw * 180 / math.Pi
		if got := lonAt(50); math.Abs(got-want) > 2.5 {
			t.Errorf("yaw %.0f°: centre shows %.1f°", want, got)
		}
		if got := lonAt(0); math.Abs(got-(want-45)) > 3 {
			t.Errorf("yaw %.0f°: left edge shows %.1f°, want %.1f", want, got, want-45)
		}
		if got := lonAt(100); math.Abs(got-(want+45)) > 3 {
			t.Errorf("yaw %.0f°: right edge shows %.1f°, want %.1f", want, got, want+45)
		}
	}
	// Level: the horizon is the middle row, and straight up is out of view.
	src = image.NewRGBA(image.Rect(0, 0, 360, 180))
	mapPixels(src, func(_, y int, _ color.RGBA) color.RGBA { return color.RGBA{0, uint8(y * 255 / 179), 0, 255} })
	v := gnomonic(src, 0, 100, 100)
	if g := v.RGBAAt(50, 50).G; g < 120 || g > 135 {
		t.Errorf("middle row shows latitude level %d, want about 128", g)
	}
	if g := v.RGBAAt(50, 0).G; g < 50 {
		t.Errorf("top row shows latitude level %d, want under 45° up", g)
	}
}

// captioned is m with a caption bar of rows burned into its bottom, or
// its top: bg with words of thin ink strokes.
func captioned(m *image.RGBA, rows int, top bool, bg, ink color.RGBA) *image.RGBA {
	h := m.Bounds().Dy()
	return mapPixels(m, func(x, y int, c color.RGBA) color.RGBA {
		i := h - 1 - y // the row in the bar, from the edge
		if top {
			i = y
		}
		if i >= rows {
			return c
		}
		t := i - rows/4 // the row in the text
		inWord := x%160 < 90 && t >= 0 && t < rows/2
		if inWord && (x%7 < 2 || (t == 0 || t == rows/4) && x%14 < 5) {
			return ink
		}
		return bg
	})
}

// shadowed is m with its bottom rows a shadowed road: dark, with grain,
// and a bright lane mark.
func shadowed(m *image.RGBA, rows int, r *rand.Rand) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	return mapPixels(m, func(x, y int, c color.RGBA) color.RGBA {
		if y < h-rows {
			return c
		}
		if x > w/2 && x < w/2+w/30 {
			return color.RGBA{200, 200, 190, 255}
		}
		l := clamp(35 + int(r.NormFloat64()*14))
		return color.RGBA{l, l, l + 3, 255}
	})
}

// skied is m with its top rows a flat, clipped sky of luma l with a
// little grain, crossed by two sagging wires and a lamp post rising into
// it, all of ink.
func skied(m *image.RGBA, rows int, l, ink uint8, r *rand.Rand) *image.RGBA {
	w := m.Bounds().Dx()
	return mapPixels(m, func(x, y int, c color.RGBA) color.RGBA {
		if y >= rows {
			return c
		}
		u := float64(x-w/2) / float64(w/2)
		for _, top := range []float64{0.3, 0.5} {
			if sag := float64(rows) * (top + 0.4*u*u); math.Abs(float64(y)-sag) < 1 {
				return color.RGBA{ink, ink, ink, 255}
			}
		}
		if x >= w*7/10 && x < w*7/10+4 && y > rows/6 {
			return color.RGBA{ink, ink, ink, 255}
		}
		v := clamp(int(l) + int(r.NormFloat64()*1.5))
		return color.RGBA{v, v, v, 255}
	})
}

// bonnet is m with its bottom rows a car's black bonnet with a bright,
// sharp reflection across a third of it.
func bonnet(m *image.RGBA, rows int, r *rand.Rand) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	return mapPixels(m, func(x, y int, c color.RGBA) color.RGBA {
		i := h - 1 - y
		switch {
		case i >= rows:
			return c
		case i >= rows/2 && i < rows/2+3 && x > w*3/10 && x < w*62/100:
			return color.RGBA{140, 140, 150, 255}
		}
		v := clamp(12 + int(r.NormFloat64()*1.5))
		return color.RGBA{v, v, v, 255}
	})
}

func TestCaptionBand(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	black, white := color.RGBA{6, 6, 6, 255}, color.RGBA{240, 240, 240, 255}
	// A vignette: the bottom darkens smoothly to near black, with a lane
	// mark, but has no edge.
	vignette := mapPixels(scene(1024, 768, r), func(x, y int, c color.RGBA) color.RGBA {
		if y < 650 {
			return c
		}
		if x > 500 && x < 540 {
			return color.RGBA{220, 220, 220, 255}
		}
		l := clamp(10 + (767-y)*90/118 + int(r.NormFloat64()*2))
		return color.RGBA{l, l, l, 255}
	})
	for _, tc := range []struct {
		name        string
		m           *image.RGBA
		top, bottom int // the rows wanted, give or take 1 for a bar
	}{
		{"none", scene(1024, 768, r), 0, 0},
		{"bottom", captioned(scene(1024, 768, r), 20, false, black, white), 0, 20 + 3},
		{"top, pale", captioned(scene(1024, 768, r), 24, true, white, black), 24 + 3, 0},
		{"both", captioned(captioned(scene(1024, 768, r), 18, true, black, white), 18, false, black, white), 18 + 3, 18 + 3},
		{"too tall", captioned(scene(1024, 768, r), 768/7, false, black, white), 0, -1},
		{"flat without text", testPhotos(1024, 768)[6].m, 0, 0},
		{"shadowed road", shadowed(scene(1024, 768, r), 80, r), 0, 0},
		{"vignette", vignette, 0, 0},
		{"a pale sky, 5%", skied(scene(1024, 768, r), 38, 250, 60, r), 0, 0},
		{"a pale sky, 20%", skied(scene(1024, 768, r), 154, 250, 60, r), 0, 0},
		{"a pale sky, 30%", skied(scene(1024, 768, r), 230, 250, 60, r), 0, 0},
		{"a night sky, 20%", skied(scene(1024, 768, r), 154, 8, 200, r), 0, 0},
		{"a bonnet, 15%", bonnet(scene(1024, 768, r), 115, r), 0, 0},
		{"a bonnet, 20%", bonnet(scene(1024, 768, r), 154, r), 0, 0},
	} {
		// As Vet sees it: through a JPEG.
		got, err := jpeg.Decode(bytes.NewReader(jpegOf(t, tc.m)))
		if err != nil {
			t.Fatal(err)
		}
		m := image.NewRGBA(got.Bounds())
		xdraw.Draw(m, m.Bounds(), got, got.Bounds().Min, xdraw.Src)
		top, bottom := captionBand(m, false), captionBand(m, true)
		// A bar's rows may be off by 1; none (0) and too tall (-1) not.
		off := func(got, want int) bool { return got != want && (want <= 0 || abs(got-want) > 1) }
		if off(top, tc.top) || off(bottom, tc.bottom) {
			t.Errorf("%s: top %d, bottom %d; want %d, %d", tc.name, top, bottom, tc.top, tc.bottom)
		}
	}
}

func abs(n int) int { return max(n, -n) }

// darkRows is the share of the pixels in m's bottom rows that are near
// black.
func darkRows(m image.Image, rows int) float64 {
	b := m.Bounds()
	dark := 0
	for y := b.Max.Y - rows; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if l := color.GrayModel.Convert(m.At(x, y)).(color.Gray).Y; l < 16 {
				dark++
			}
		}
	}
	return float64(dark) / float64(rows*b.Dx())
}

func TestVetCaption(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(13, 14))
	black, white := color.RGBA{6, 6, 6, 255}, color.RGBA{240, 240, 240, 255}
	vet := func(name string, m *image.RGBA) (image.Image, error) {
		out, err := Vet(jpegOf(t, m), false, r)
		if err != nil {
			return nil, err
		}
		got, _, err := image.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return got, nil
	}

	// A real photo, scaled down for the test data and back up for Vet:
	// a Lima street by Kaart (Mapillary, CC BY-SA 4.0) with its country,
	// city, time, author and position burned into a black bar along the
	// bottom, about 2.5% of the height.
	data, err := os.ReadFile("testdata/caption.jpg")
	if err != nil {
		t.Fatal(err)
	}
	small, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if darkRows(small, 3) < 0.5 {
		t.Fatal("the test photo has no caption bar")
	}
	m, err := vet("Lima", scale(small, 1600, 1600*small.Bounds().Dy()/small.Bounds().Dx(), xdraw.CatmullRom))
	if err != nil {
		t.Fatalf("Lima: %v", err)
	}
	if h := m.Bounds().Dy(); m.Bounds().Dx() != 1600 || h < 894*95/100 || h > 894*985/1000 {
		t.Errorf("Lima: %v, want 1600 wide with about 2.5%% cut", m.Bounds())
	}
	if d := darkRows(m, 3); d > 0.3 {
		t.Errorf("Lima: %.0f%% of the bottom rows are black", 100*d)
	}

	for _, tc := range []struct {
		name string
		m    *image.RGBA
		h    int // the height wanted, give or take 2; 0 for rejected
	}{
		{"bottom", captioned(scene(1024, 768, r), 20, false, black, white), 768 - 23},
		{"top", captioned(scene(1024, 768, r), 20, true, black, white), 768 - 23},
		{"too tall", captioned(scene(1024, 768, r), 100, false, black, white), 0},
		{"shadowed road", shadowed(scene(1024, 768, r), 80, r), 768},
	} {
		m, err := vet(tc.name, tc.m)
		switch {
		case tc.h == 0 && err != RejectCaption:
			t.Errorf("%s: %v, want %v", tc.name, err, RejectCaption)
		case tc.h == 0:
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case abs(m.Bounds().Dy()-tc.h) > 2 || darkRows(m, 3) > 0.3:
			t.Errorf("%s: %v, %.0f%% dark at the bottom; want %d high", tc.name, m.Bounds(), 100*darkRows(m, 3), tc.h)
		}
	}
	// Skies and a bonnet are no caption, whatever else Vet makes of them.
	for _, tc := range []struct {
		name string
		m    *image.RGBA
	}{
		{"a pale sky", skied(scene(1024, 768, r), 154, 250, 60, r)},
		{"a night sky", skied(scene(1024, 768, r), 154, 8, 200, r)},
		{"a bonnet", bonnet(scene(1024, 768, r), 115, r)},
	} {
		if _, err := vet(tc.name, tc.m); err == RejectCaption {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
