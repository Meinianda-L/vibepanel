package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

// toRGBA turns a 32-bit ZPixmap into an image. X sends B, G, R, pad in
// LSB-first order, which is every x86 server; MSB-first is pad, R, G, B.
func toRGBA(data []byte, w, h int, lsb bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	px := img.Pix
	n := min(len(data)/4, w*h)
	for i := 0; i < n; i++ {
		s := data[i*4 : i*4+4]
		d := px[i*4 : i*4+4]
		if lsb {
			d[0], d[1], d[2] = s[2], s[1], s[0]
		} else {
			d[0], d[1], d[2] = s[1], s[2], s[3]
		}
		d[3] = 0xff
	}
	return img
}

// FitWidth is the size an image of w×h becomes when it is made at most maxW
// wide (and at most maxH tall, when maxH > 0), keeping its shape.
func FitWidth(w, h, maxW, maxH int) (int, int) {
	if maxW <= 0 || maxW > w {
		maxW = w
	}
	nw, nh := maxW, h*maxW/w
	if maxH > 0 && nh > maxH {
		nh = maxH
		nw = w * maxH / h
	}
	return max(1, nw), max(1, nh)
}

// Resize scales src to dw×dh by averaging the source pixels each destination
// pixel covers -- a box filter, separable, fractional at the edges.
//
// Averaging rather than sampling because the main reader is a model reading
// small text off the screen: point sampling a 1920-wide screen down to 1280
// drops one column in three and turns 11-pixel type into something nobody,
// human or not, reads reliably.
func Resize(src *image.RGBA, dw, dh int) *image.RGBA {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if dw == sw && dh == sh {
		return src
	}
	// Horizontal pass into a float buffer, then vertical.
	tmp := make([]float32, dw*sh*3)
	wx := boxWeights(sw, dw)
	for y := 0; y < sh; y++ {
		row := src.Pix[y*src.Stride:]
		for x := 0; x < dw; x++ {
			var r, g, b float32
			for _, t := range wx[x] {
				p := row[t.i*4:]
				r += float32(p[0]) * t.w
				g += float32(p[1]) * t.w
				b += float32(p[2]) * t.w
			}
			o := (y*dw + x) * 3
			tmp[o], tmp[o+1], tmp[o+2] = r, g, b
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	wy := boxWeights(sh, dh)
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var r, g, b float32
			for _, t := range wy[y] {
				o := (t.i*dw + x) * 3
				r += tmp[o] * t.w
				g += tmp[o+1] * t.w
				b += tmp[o+2] * t.w
			}
			d := dst.Pix[y*dst.Stride+x*4:]
			d[0], d[1], d[2], d[3] = clamp8(r), clamp8(g), clamp8(b), 0xff
		}
	}
	return dst
}

type tap struct {
	i int
	w float32
}

// boxWeights: for each of n destination pixels, which of m source pixels it
// covers and by how much, summing to 1.
func boxWeights(m, n int) [][]tap {
	out := make([][]tap, n)
	scale := float64(m) / float64(n)
	for j := 0; j < n; j++ {
		lo, hi := float64(j)*scale, float64(j+1)*scale
		var taps []tap
		for i := int(lo); i < m && float64(i) < hi; i++ {
			a := max(lo, float64(i))
			b := min(hi, float64(i+1))
			if b > a {
				taps = append(taps, tap{i: i, w: float32((b - a) / scale)})
			}
		}
		out[j] = taps
	}
	return out
}

func clamp8(v float32) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

// JPEG encodes at a quality.
func JPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// signature is a cheap fingerprint of a frame, to skip sending one that has
// not changed: a hash of a sparse grid of pixels. A change smaller than the
// grid -- a blinking caret between sample points -- can be missed for a
// frame; the next refresh that touches a sampled pixel sends it, and a
// keyframe goes out every few seconds regardless.
func signature(img *image.RGBA) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	w, ht := img.Rect.Dx(), img.Rect.Dy()
	for y := 0; y < ht; y += 3 {
		row := img.Pix[y*img.Stride:]
		for x := (y / 3) % 5; x < w; x += 5 {
			p := row[x*4:]
			h = (h ^ uint64(p[0])) * prime
			h = (h ^ uint64(p[1])) * prime
			h = (h ^ uint64(p[2])) * prime
		}
	}
	return h
}

// ShotRGBA is the screen reduced to w×h for the agent, straight from the
// BGRX buffer: the box filter of Resize, with the colour conversion folded in
// and fixed-point weights, and without first building a full-size RGBA copy.
// On the Core 2 Duo that copy and the float resize were 60 ms of every
// action's screenshot.
func ShotRGBA(src []byte, sw, sh int, lsb bool, dw, dh int) *image.RGBA {
	if lsb && dw*3 == sw*2 && dh*3 == sh*2 {
		return shotTwoThirds(src, sw, dw, dh)
	}
	stride := sw * 4
	ri, gi, bi := 2, 1, 0
	if !lsb {
		ri, gi, bi = 1, 2, 3
	}
	const one = 1 << 16
	type itap struct {
		i int
		w uint32
	}
	fixed := func(m, n int) [][]itap {
		out := make([][]itap, n)
		for j, taps := range boxWeights(m, n) {
			var sum uint32
			for k, t := range taps {
				w := uint32(t.w*one + 0.5)
				if k == len(taps)-1 {
					w = one - sum // exact: the weights of a pixel sum to one
				}
				sum += w
				out[j] = append(out[j], itap{t.i, w})
			}
		}
		return out
	}
	wx, wy := fixed(sw, dw), fixed(sh, dh)
	// Each source row's horizontal sums are made once, when the first
	// destination row that needs them does, and kept only while a later one
	// still might: rows are consumed in order and a box covers at most a few,
	// so a small ring of them stands in for a whole-image buffer -- which at
	// 16 MB was most of this function's time on a machine with 3 MB of cache.
	const ring = 4
	sums := make([][]uint32, ring)
	for i := range sums {
		sums[i] = make([]uint32, dw*3)
	}
	have := [ring]int{-1, -1, -1, -1}
	hsum := func(y int) []uint32 {
		slot := y % ring
		if have[slot] == y {
			return sums[slot]
		}
		s := src[y*stride : y*stride+sw*4]
		o := sums[slot]
		for x, taps := range wx {
			var r, g, b uint32
			for _, t := range taps {
				i := t.i * 4
				r += uint32(s[i+ri]) * t.w
				g += uint32(s[i+gi]) * t.w
				b += uint32(s[i+bi]) * t.w
			}
			o[x*3], o[x*3+1], o[x*3+2] = r>>8, g>>8, b>>8
		}
		have[slot] = y
		return o
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	acc := make([]uint64, dw*3)
	for y, taps := range wy {
		for i := range acc {
			acc[i] = 0
		}
		for _, t := range taps {
			o := hsum(t.i)
			w := uint64(t.w)
			for i, v := range o {
				acc[i] += uint64(v) * w
			}
		}
		d := dst.Pix[y*dst.Stride : y*dst.Stride+dw*4]
		for x := 0; x < dw; x++ {
			d[x*4] = uint8(min(255, (acc[x*3]+(1<<23))>>24))
			d[x*4+1] = uint8(min(255, (acc[x*3+1]+(1<<23))>>24))
			d[x*4+2] = uint8(min(255, (acc[x*3+2]+(1<<23))>>24))
			d[x*4+3] = 0xff
		}
	}
	return dst
}

// shotTwoThirds is ShotRGBA for exactly 2/3 in both directions, which is a
// 1920x1080 screen for an agent that sees 1280x720 -- the common case. Every
// 3x3 block of source pixels makes a 2x2 block of output, and the box filter
// there is fixed: a corner output takes its corner pixel at 4/9, the two edge
// pixels next to it at 2/9 each, and the centre at 1/9. Written out with no
// tap tables it was 60 ms down to a fraction on the Core 2 Duo.
func shotTwoThirds(src []byte, sw, dw, dh int) *image.RGBA {
	stride := sw * 4
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	px := func(row []byte, i int) (uint32, uint32, uint32) {
		v := binary.LittleEndian.Uint32(row[i*4:])
		return (v >> 16) & 0xff, (v >> 8) & 0xff, v & 0xff
	}
	put := func(d []byte, x int, r, g, b uint32) {
		// Weights sum to 9; +4 rounds.
		binary.LittleEndian.PutUint32(d[x*4:], 0xff000000|((b+4)/9)<<16|((g+4)/9)<<8|(r+4)/9)
	}
	for by := 0; by < dh/2; by++ {
		r0 := src[(3*by)*stride : (3*by)*stride+sw*4]
		r1 := src[(3*by+1)*stride : (3*by+1)*stride+sw*4]
		r2 := src[(3*by+2)*stride : (3*by+2)*stride+sw*4]
		d0 := dst.Pix[(2*by)*dst.Stride : (2*by)*dst.Stride+dw*4]
		d1 := dst.Pix[(2*by+1)*dst.Stride : (2*by+1)*dst.Stride+dw*4]
		for bx := 0; bx < dw/2; bx++ {
			i := 3 * bx
			ar, ag, ab := px(r0, i)
			br, bg, bb := px(r0, i+1)
			cr, cg, cb := px(r0, i+2)
			dr, dg, db := px(r1, i)
			er, eg, eb := px(r1, i+1)
			fr, fg, fb := px(r1, i+2)
			gr, gg, gb := px(r2, i)
			hr, hg, hb := px(r2, i+1)
			ir, ig, ib := px(r2, i+2)
			put(d0, 2*bx, 4*ar+2*br+2*dr+er, 4*ag+2*bg+2*dg+eg, 4*ab+2*bb+2*db+eb)
			put(d0, 2*bx+1, 4*cr+2*br+2*fr+er, 4*cg+2*bg+2*fg+eg, 4*cb+2*bb+2*fb+eb)
			put(d1, 2*bx, 4*gr+2*hr+2*dr+er, 4*gg+2*hg+2*dg+eg, 4*gb+2*hb+2*db+eb)
			put(d1, 2*bx+1, 4*ir+2*hr+2*fr+er, 4*ig+2*hg+2*fg+eg, 4*ib+2*hb+2*fb+eb)
		}
	}
	return dst
}
