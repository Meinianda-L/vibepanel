package desktop

import (
	"image"
	"os"
	"testing"
)

// Benchmarks against a real display, for deciding what is worth making
// faster on the machine that will run it. Skipped unless
// VIBEPANEL_DESKTOP_TEST names a display.

func benchDesktop(b *testing.B) *Desktop {
	disp := os.Getenv("VIBEPANEL_DESKTOP_TEST")
	if disp == "" {
		b.Skip("VIBEPANEL_DESKTOP_TEST is not set")
	}
	d := Open(disp)
	if _, _, err := d.Size(); err != nil {
		b.Fatal(err)
	}
	return d
}

func BenchmarkCapture(b *testing.B) {
	d := benchDesktop(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Capture(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchFrame(b *testing.B) *image.RGBA {
	d := benchDesktop(b)
	img, err := d.Capture()
	if err != nil {
		b.Fatal(err)
	}
	return img
}

func BenchmarkSignature(b *testing.B) {
	img := benchFrame(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		signature(img)
	}
}

func BenchmarkResizeHalf(b *testing.B) {
	img := benchFrame(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Resize(img, img.Rect.Dx()/2, img.Rect.Dy()/2)
	}
}

func BenchmarkJPEGFull(b *testing.B) {
	img := benchFrame(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := JPEG(img, streamQuality); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJPEGHalf(b *testing.B) {
	img := benchFrame(b)
	half := Resize(img, img.Rect.Dx()/2, img.Rect.Dy()/2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := JPEG(half, streamQuality); err != nil {
			b.Fatal(err)
		}
	}
}

// The fast path, stage by stage.

func BenchmarkGrabFullSHM(b *testing.B) {
	d := benchDesktop(b)
	w, h, _ := d.Size()
	d.grabMu.Lock()
	defer d.grabMu.Unlock()
	if d.grabber == nil {
		b.Skip("no MIT-SHM on this display")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.grabLocked(0, 0, w, h); err != nil {
			b.Fatal(err)
		}
	}
}

// A line of terminal text: 640x64.
func BenchmarkGrabLineSHM(b *testing.B) {
	d := benchDesktop(b)
	d.grabMu.Lock()
	defer d.grabMu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := d.grabLocked(0, 512, 640, 64); err != nil {
			b.Fatal(err)
		}
	}
}

func benchCur(b *testing.B) (*Desktop, int, int) {
	d := benchDesktop(b)
	w, h, _ := d.Size()
	d.grabMu.Lock()
	err := d.grabLocked(0, 0, w, h)
	d.grabMu.Unlock()
	if err != nil {
		b.Fatal(err)
	}
	return d, w, h
}

func BenchmarkScaleHalfFull(b *testing.B) {
	d, w, h := benchCur(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scaleBGRX(d.cur, w*4, d.lsb, rect{0, 0, w, h}, 2)
	}
}

// What one keystroke in a terminal costs to send at half scale: a 640x64
// strip, scaled and encoded.
func BenchmarkEncodeLineHalf(b *testing.B) {
	d, w, _ := benchCur(b)
	rs := []rect{{0, 512, 640, 64}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encodeRects(nil, d.cur, w*4, d.lsb, rs, 2)
	}
}

// A keyframe at half scale, which a viewer gets on joining.
func BenchmarkKeyframeHalf(b *testing.B) {
	d, w, h := benchCur(b)
	rs := []rect{{0, 0, w, h}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		encodeRects(nil, d.cur, w*4, d.lsb, rs, 2)
	}
}

func BenchmarkAgentShot(b *testing.B) {
	d := benchDesktop(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		img, _, _, err := d.Shot(1280, 800)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := JPEG(img, 80); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAgentShotResizeOnly(b *testing.B) {
	d, w, h := benchCur(b)
	dw, dh := FitWidth(w, h, 1280, 800)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ShotRGBA(d.cur, w, h, d.lsb, dw, dh)
	}
}

func BenchmarkAgentShotJPEGOnly(b *testing.B) {
	d, w, h := benchCur(b)
	dw, dh := FitWidth(w, h, 1280, 800)
	img := ShotRGBA(d.cur, w, h, d.lsb, dw, dh)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := JPEG(img, 80); err != nil {
			b.Fatal(err)
		}
	}
}
