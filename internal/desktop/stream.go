package desktop

import (
	"bytes"
	"encoding/binary"
	"image"
	"sync"
	"time"
)

// The live view, sent the way remote-desktop protocols send it rather than
// as video: what changed, as JPEG rectangles, which the browser draws onto
// its copy of the screen.
//
// Not video because of the machine it runs on. A software H.264 encoder at
// 1080p is two cores' worth of a 2008 Core 2 Duo, and that machine has no
// hardware encoder; a desktop is also mostly still, and an encoder that
// re-describes the whole frame thirty times a second spends nearly all of
// that saying nothing changed. Here an idle screen costs nothing at all --
// the loop sleeps until the server reports damage -- and a busy one costs in
// proportion to the area that is busy: a line of terminal text is a couple of
// 64-pixel tiles.
//
// The pipeline, each step there because of a measurement on that machine:
//
//   - damage: the X server says which rectangles changed (DAMAGE extension),
//     so nothing is captured to find out;
//   - capture only those rectangles, through shared memory (MIT-SHM): a
//     whole-screen GetImage over the socket was 95 ms and 25 MB a time;
//   - compare each damaged tile with what the viewers were last sent, because
//     damage over-reports -- a toolkit repainting a button identically is
//     damage too -- and only send the tiles whose pixels differ;
//   - scale and convert in one integer pass at 1/1, 1/2 or 1/4, where the
//     general resize was 47 ms a frame;
//   - and adapt: the gap between frames grows with how long the last one took
//     to encode, so a screen that is all motion (a video, a dragged window)
//     gets fewer frames rather than all of the CPU.

// Record kinds in a binary message; see encodeRecord.
const (
	recStart   = 1 // a keyframe follows: scale, screen size, view size
	recTile    = 2 // x, y, w, h in view pixels, then a JPEG
	recPointer = 3 // the pointer, in screen pixels
)

const (
	minFrameGap   = 33 * time.Millisecond // 30 frames a second at most
	maxFrameGap   = 400 * time.Millisecond
	pointerEvery  = 50 * time.Millisecond
	pointerIdle   = 250 * time.Millisecond
	coalesce      = 12 * time.Millisecond // let a burst of damage finish
	pollEvery     = 500 * time.Millisecond
	streamQuality = 72
	viewerBacklog = 2
)

type viewer struct {
	scale int
	ch    chan []byte
	fresh bool // needs a keyframe
}

type streamHub struct {
	d       *Desktop
	mu      sync.Mutex
	viewers map[*viewer]struct{}
	run     bool
	stats   StreamStats
}

// StreamStats is where a stream's time goes, for tuning on the machine that
// runs it and for the tests that measure that.
type StreamStats struct {
	Frames      int
	WholeScreen int // frames where damage was too scattered to list
	DamageRects int
	Captured    int // pixels captured
	Changed     int // tiles that really differed
	Grab        time.Duration
	Diff        time.Duration
	Encode      time.Duration
}

// Stats is a copy of the stream's counters so far.
func (d *Desktop) Stats() StreamStats {
	d.frames.mu.Lock()
	defer d.frames.mu.Unlock()
	return d.frames.stats
}

func newStreamHub(d *Desktop) *streamHub {
	return &streamHub{d: d, viewers: map[*viewer]struct{}{}}
}

func (h *streamHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

// scaleFor is the largest reduction that still gives the viewer at least the
// width it asked for, among 1, 2 and 4. Integer factors are what make the
// one-pass scale possible.
func scaleFor(screenW, want int) int {
	switch {
	case want <= 0 || want*2 > screenW:
		return 1
	case want*4 > screenW:
		return 2
	default:
		return 4
	}
}

func (h *streamHub) subscribe(width int) (<-chan []byte, func()) {
	sw, _, _ := h.d.Size()
	v := &viewer{scale: scaleFor(sw, width), ch: make(chan []byte, viewerBacklog), fresh: true}
	h.mu.Lock()
	h.viewers[v] = struct{}{}
	if !h.run {
		h.run = true
		go h.loop()
	}
	h.mu.Unlock()
	var once sync.Once
	return v.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.viewers, v)
			h.mu.Unlock()
		})
	}
}

func (h *streamHub) loop() {
	d := h.d
	tick := time.NewTicker(pointerEvery)
	defer tick.Stop()
	var (
		grid      *tiles
		changed   *tiles
		rects     []rect
		damage    []rect
		lastFrame time.Time
		lastPoll  time.Time
		gap       = minFrameGap
		px, py    = -1, -1

		pointerMoved time.Time
		pointerAsked time.Time
		pending      bool
	)
	for {
		h.mu.Lock()
		if len(h.viewers) == 0 {
			h.run = false
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()

		d.mu.Lock()
		q := d.damaged
		d.mu.Unlock()
		var wake <-chan struct{}
		if q != nil {
			wake = q.wake
		}
		// Damage that arrived too soon after the last frame is not left for
		// the next event to pick up: a timer fires when the gap is up. Without
		// it an animation at 30 frames a second streamed at 18, every frame
		// that landed inside the gap waiting for the one after.
		var due <-chan time.Time
		if pending {
			due = time.After(max(0, gap-time.Since(lastFrame)))
		}
		select {
		case <-wake:
			pending = true
			time.Sleep(coalesce)
		case <-due:
		case <-tick.C:
		}

		w, ht, err := d.Size()
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if grid == nil || grid.w != w || grid.h != ht {
			grid, changed = newTiles(w, ht), newTiles(w, ht)
			h.markAllFresh()
		}

		// The pointer goes out on its own clock: moving it is not damage.
		// Asked every tick while it is moving and four times a second once it
		// has been still for a second -- a round trip to the server per ask
		// was most of what watching an idle screen cost.
		var out []byte
		if time.Since(pointerMoved) < time.Second || time.Since(pointerAsked) >= pointerIdle {
			pointerAsked = time.Now()
			if x, y, err := d.Pointer(); err == nil && (x != px || y != py) {
				px, py = x, y
				pointerMoved = time.Now()
				out = appendPointer(out, x, y)
			}
		}

		viewers, anyFresh := h.snapshot()
		if time.Since(lastFrame) >= gap {
			pending = false
			grid.clear()
			if q != nil {
				var all bool
				damage, all = q.take(damage)
				if all {
					grid.markAll()
				}
				h.mu.Lock()
				h.stats.DamageRects += len(damage)
				if all {
					h.stats.WholeScreen++
				}
				h.mu.Unlock()
				for _, r := range damage {
					grid.markRect(r.x, r.y, r.w, r.h)
				}
			} else if time.Since(lastPoll) >= pollEvery {
				// No DAMAGE extension: look at everything, twice a second, and
				// let the tile comparison find what changed.
				grid.markAll()
				lastPoll = time.Now()
			}
			if anyFresh {
				grid.markAll()
			}
			if grid.any() {
				start := time.Now()
				frames := h.frame(grid, changed, viewers, &rects)
				spent := time.Since(start)
				lastFrame = time.Now()
				// Twice what the last frame cost, within bounds: the loop then
				// spends at most about half its time encoding, which leaves the
				// machine to the agent and everything else on it.
				gap = min(maxFrameGap, max(minFrameGap, 2*spent))
				h.send(viewers, frames, out)
				continue
			}
		}
		if len(out) > 0 {
			h.send(viewers, nil, out)
		}
	}
}

func (h *streamHub) markAllFresh() {
	h.mu.Lock()
	for v := range h.viewers {
		v.fresh = true
	}
	h.mu.Unlock()
}

func (h *streamHub) snapshot() ([]*viewer, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*viewer, 0, len(h.viewers))
	fresh := false
	for v := range h.viewers {
		out = append(out, v)
		fresh = fresh || v.fresh
	}
	return out, fresh
}

// frame captures the marked tiles, works out which really changed, and
// encodes what each viewer needs: a keyframe for a fresh one, the changed
// rectangles for the rest. Encoding happens once per scale however many
// viewers share it.
func (h *streamHub) frame(grid, changed *tiles, viewers []*viewer, rects *[]rect) map[*viewer][]byte {
	d := h.d
	d.mu.Lock()
	w, ht, lsb := d.width, d.height, d.lsb
	d.mu.Unlock()
	stride := w * 4

	d.grabMu.Lock()
	defer d.grabMu.Unlock()
	t0 := time.Now()
	*rects = grid.rects(*rects)
	captured := 0
	for _, r := range *rects {
		if err := d.grabLocked(r.x, r.y, r.w, r.h); err != nil {
			return nil
		}
		captured += r.w * r.h
	}
	t1 := time.Now()
	// Which captured tiles differ from what was last sent, and bring the sent
	// copy up to date with them.
	changed.clear()
	for ty := 0; ty < grid.rows; ty++ {
		for tx := 0; tx < grid.cols; tx++ {
			if !grid.mark[ty*grid.cols+tx] {
				continue
			}
			x0, y0 := tx*tileSize, ty*tileSize
			x1, y1 := min(w, x0+tileSize), min(ht, y0+tileSize)
			diff := false
			for y := y0; y < y1; y++ {
				a := d.cur[y*stride+x0*4 : y*stride+x1*4]
				b := d.prev[y*stride+x0*4 : y*stride+x1*4]
				if !bytes.Equal(a, b) {
					diff = true
					break
				}
			}
			if diff {
				changed.mark[ty*changed.cols+tx] = true
				for y := y0; y < y1; y++ {
					copy(d.prev[y*stride+x0*4:y*stride+x1*4], d.cur[y*stride+x0*4:y*stride+x1*4])
				}
			}
		}
	}
	deltaRects := changed.rects(nil)
	full := []rect{{0, 0, w, ht}}
	t2 := time.Now()
	nChanged := 0
	for _, m := range changed.mark {
		if m {
			nChanged++
		}
	}
	defer func() {
		h.mu.Lock()
		h.stats.Frames++
		h.stats.Captured += captured
		h.stats.Changed += nChanged
		h.stats.Grab += t1.Sub(t0)
		h.stats.Diff += t2.Sub(t1)
		h.stats.Encode += time.Since(t2)
		h.mu.Unlock()
	}()

	delta := map[int][]byte{}
	key := map[int][]byte{}
	out := map[*viewer][]byte{}
	for _, v := range viewers {
		h.mu.Lock()
		fresh := v.fresh
		h.mu.Unlock()
		if fresh {
			b, ok := key[v.scale]
			if !ok {
				b = appendStart(nil, v.scale, w, ht)
				b = encodeRects(b, d.cur, stride, lsb, full, v.scale)
				key[v.scale] = b
			}
			out[v] = b
			h.mu.Lock()
			v.fresh = false
			h.mu.Unlock()
			continue
		}
		if len(deltaRects) == 0 {
			continue
		}
		b, ok := delta[v.scale]
		if !ok {
			b = encodeRects(nil, d.cur, stride, lsb, deltaRects, v.scale)
			delta[v.scale] = b
		}
		out[v] = b
	}
	return out
}

// send hands each viewer its frame and the pointer. A viewer whose backlog is
// full is not queued for: it is marked fresh and gets a keyframe when it has
// caught up, which on a live view is worth more than every frame it missed.
func (h *streamHub) send(viewers []*viewer, frames map[*viewer][]byte, pointer []byte) {
	for _, v := range viewers {
		msg := frames[v]
		if len(pointer) > 0 {
			msg = append(append([]byte(nil), msg...), pointer...)
		}
		if len(msg) == 0 {
			continue
		}
		select {
		case v.ch <- msg:
		default:
			h.mu.Lock()
			v.fresh = true
			h.mu.Unlock()
		}
	}
}

func appendRecord(b []byte, kind byte, payload ...[]byte) []byte {
	n := 0
	for _, p := range payload {
		n += len(p)
	}
	b = append(b, kind)
	b = binary.BigEndian.AppendUint32(b, uint32(n))
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

func u16s(vs ...int) []byte {
	out := make([]byte, 0, len(vs)*2)
	for _, v := range vs {
		out = binary.BigEndian.AppendUint16(out, uint16(max(0, v)))
	}
	return out
}

func appendStart(b []byte, scale, sw, sh int) []byte {
	return appendRecord(b, recStart, []byte{byte(scale)}, u16s(sw, sh, sw/scale, sh/scale))
}

func appendPointer(b []byte, x, y int) []byte {
	return appendRecord(b, recPointer, u16s(x, y))
}

// encodeRects scales and JPEG-encodes each rectangle and appends it as a
// tile record.
func encodeRects(b []byte, src []byte, stride int, lsb bool, rs []rect, k int) []byte {
	for _, r := range rs {
		img := scaleBGRX(src, stride, lsb, r, k)
		if img == nil {
			continue
		}
		j, err := JPEG(img, streamQuality)
		if err != nil {
			continue
		}
		b = appendRecord(b, recTile, u16s(r.x/k, r.y/k, img.Rect.Dx(), img.Rect.Dy()), j)
	}
	return b
}

// scaleBGRX converts a rectangle of the BGRX screen to RGBA and reduces it
// by k in one pass, averaging each k×k block with integer arithmetic.
//
// 1 and 2 have loops of their own because they are nearly every frame: the
// general loop slices every pixel and pays a bounds check for each, which was
// 27 ms for a whole screen at half size on the Core 2 Duo; these read four
// bytes at a time from rows sliced once.
func scaleBGRX(src []byte, stride int, lsb bool, r rect, k int) *image.RGBA {
	ow, oh := r.w/k, r.h/k
	if ow <= 0 || oh <= 0 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, ow, oh))
	switch {
	case k == 1 && lsb:
		for y := 0; y < oh; y++ {
			s := src[(r.y+y)*stride+r.x*4 : (r.y+y)*stride+(r.x+ow)*4]
			d := img.Pix[y*img.Stride : y*img.Stride+ow*4]
			for i := 0; i+3 < len(s) && i+3 < len(d); i += 4 {
				v := binary.LittleEndian.Uint32(s[i:])
				// B G R X in memory is 0xXXRRGGBB as a little-endian word;
				// R G B A is 0xAABBGGRR.
				binary.LittleEndian.PutUint32(d[i:], 0xff000000|(v>>16)&0xff|v&0xff00|(v&0xff)<<16)
			}
		}
		return img
	case k == 2 && lsb:
		for y := 0; y < oh; y++ {
			row := (r.y + 2*y) * stride
			a := src[row+r.x*4 : row+(r.x+2*ow)*4]
			b := src[row+stride+r.x*4 : row+stride+(r.x+2*ow)*4]
			d := img.Pix[y*img.Stride : y*img.Stride+ow*4]
			for x := 0; x < ow; x++ {
				i := x * 8
				if i+7 >= len(a) || i+7 >= len(b) {
					break
				}
				p0 := binary.LittleEndian.Uint32(a[i:])
				p1 := binary.LittleEndian.Uint32(a[i+4:])
				p2 := binary.LittleEndian.Uint32(b[i:])
				p3 := binary.LittleEndian.Uint32(b[i+4:])
				red := ((p0>>16)&0xff + (p1>>16)&0xff + (p2>>16)&0xff + (p3>>16)&0xff) >> 2
				grn := ((p0>>8)&0xff + (p1>>8)&0xff + (p2>>8)&0xff + (p3>>8)&0xff) >> 2
				blu := (p0&0xff + p1&0xff + p2&0xff + p3&0xff) >> 2
				binary.LittleEndian.PutUint32(d[x*4:], 0xff000000|blu<<16|grn<<8|red)
			}
		}
		return img
	}
	ri, gi, bi := 2, 1, 0
	if !lsb {
		ri, gi, bi = 1, 2, 3
	}
	n := uint32(k * k)
	for y := 0; y < oh; y++ {
		d := img.Pix[y*img.Stride:]
		for x := 0; x < ow; x++ {
			var sr, sg, sb uint32
			for dy := 0; dy < k; dy++ {
				s := src[(r.y+y*k+dy)*stride+(r.x+x*k)*4:]
				for dx := 0; dx < k; dx++ {
					p := s[dx*4 : dx*4+4]
					sr += uint32(p[ri])
					sg += uint32(p[gi])
					sb += uint32(p[bi])
				}
			}
			q := d[x*4 : x*4+4]
			q[0], q[1], q[2], q[3] = uint8(sr/n), uint8(sg/n), uint8(sb/n), 0xff
		}
	}
	return img
}
