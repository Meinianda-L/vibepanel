package desktop

import "sync"

// damageQueue collects the rectangles the server reports changed, between
// frames. It is written by the event reader on every drawing operation the
// server does, so it does as little as possible there: append, and wake the
// stream.
type damageQueue struct {
	mu    sync.Mutex
	rects []rect
	// Too many to be worth listing: treat the whole screen as changed.
	all  bool
	wake chan struct{}
}

type rect struct{ x, y, w, h int }

// maxDamageRects is where listing rectangles stops paying. A window being
// dragged or a video playing reports hundreds a frame, and past this the
// tiles they cover are most of the screen anyway.
const maxDamageRects = 256

func newDamageQueue() *damageQueue {
	return &damageQueue{wake: make(chan struct{}, 1)}
}

func (q *damageQueue) add(x, y, w, h int) {
	q.mu.Lock()
	if !q.all {
		if len(q.rects) >= maxDamageRects {
			q.all = true
			q.rects = q.rects[:0]
		} else {
			q.rects = append(q.rects, rect{x, y, w, h})
		}
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns what changed since the last take and empties the queue.
func (q *damageQueue) take(buf []rect) ([]rect, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	all := q.all
	buf = append(buf[:0], q.rects...)
	q.rects = q.rects[:0]
	q.all = false
	return buf, all
}

// tiles is the screen cut into fixed squares, and which of them are marked.
//
// 64 pixels: small enough that a line of terminal text or a blinking caret is
// a few tiles rather than a band of the screen, large enough that a JPEG of
// one is mostly picture rather than header. Divisible by every scale the view
// is sent at (1, 2, 4), so a tile is a whole number of pixels at each.
const tileSize = 64

type tiles struct {
	cols, rows int
	w, h       int
	mark       []bool
}

func newTiles(w, h int) *tiles {
	c := (w + tileSize - 1) / tileSize
	r := (h + tileSize - 1) / tileSize
	return &tiles{cols: c, rows: r, w: w, h: h, mark: make([]bool, c*r)}
}

func (t *tiles) clear() {
	for i := range t.mark {
		t.mark[i] = false
	}
}

func (t *tiles) markAll() {
	for i := range t.mark {
		t.mark[i] = true
	}
}

// markRect marks every tile a rectangle touches.
func (t *tiles) markRect(x, y, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	x0 := max(0, x) / tileSize
	y0 := max(0, y) / tileSize
	x1 := min(t.w-1, x+w-1) / tileSize
	y1 := min(t.h-1, y+h-1) / tileSize
	for ty := y0; ty <= y1 && ty < t.rows; ty++ {
		for tx := x0; tx <= x1 && tx < t.cols; tx++ {
			t.mark[ty*t.cols+tx] = true
		}
	}
}

func (t *tiles) any() bool {
	for _, m := range t.mark {
		if m {
			return true
		}
	}
	return false
}

// rects turns the marked tiles into few rectangles, in pixels: runs along
// each row, then runs on consecutive rows with the same span joined. Fewer
// rectangles is fewer captures and fewer JPEG headers; the cost is the odd
// unmarked tile inside one, which is never the case for a run.
func (t *tiles) rects(out []rect) []rect {
	out = out[:0]
	type span struct{ x0, x1, y0, y1 int } // tile units, inclusive
	var open []span
	for ty := 0; ty < t.rows; ty++ {
		var row []span
		for tx := 0; tx < t.cols; {
			if !t.mark[ty*t.cols+tx] {
				tx++
				continue
			}
			s := tx
			for tx < t.cols && t.mark[ty*t.cols+tx] {
				tx++
			}
			row = append(row, span{s, tx - 1, ty, ty})
		}
		// Extend a span from the row above when this row has the same one.
		var next []span
		for _, r := range row {
			joined := false
			for i := range open {
				if open[i].x0 == r.x0 && open[i].x1 == r.x1 && open[i].y1 == ty-1 {
					open[i].y1 = ty
					next = append(next, open[i])
					open[i].x0 = -1
					joined = true
					break
				}
			}
			if !joined {
				next = append(next, r)
			}
		}
		for _, o := range open {
			if o.x0 >= 0 {
				out = append(out, t.pixels(o.x0, o.y0, o.x1, o.y1))
			}
		}
		open = next
	}
	for _, o := range open {
		out = append(out, t.pixels(o.x0, o.y0, o.x1, o.y1))
	}
	return out
}

func (t *tiles) pixels(x0, y0, x1, y1 int) rect {
	px, py := x0*tileSize, y0*tileSize
	return rect{px, py, min(t.w, (x1+1)*tileSize) - px, min(t.h, (y1+1)*tileSize) - py}
}
