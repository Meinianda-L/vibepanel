package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/jiangmuran/vibepanel/internal/config"
	"github.com/jiangmuran/vibepanel/internal/desktop"
)

// The desktop: a live view of an X11 display on this machine, which a person
// at the panel can watch, take over and stop, and which an agent can operate
// through `vibepanel desktop-mcp`. internal/desktop has the why of the
// approach; this is how it reaches a browser and an agent.
//
// Two surfaces and two credentials, kept apart the way share links are
// (red line 8):
//
//   - The person's routes are under RequireAuth like everything else: the
//     view, the status, Stop and Resume, and the person's own input.
//   - The agent's routes are /api/desktop/agent/*, two of them, behind a
//     token minted when the panel starts and written to a 0600 file in the
//     data directory for the MCP server to read. currentUser does not know
//     that token, so it opens nothing else; and the session cookie does not
//     open the agent's routes, so a page that tricks a browser into a request
//     cannot act as the agent.
//
// What the agent does goes to the audit log, one line per action, because a
// program clicking around somebody's screen is exactly what they will want a
// record of afterwards.

// agentMaxWidth/Height bound the screenshots the agent sees, and so the
// coordinate space it acts in. About 1280 wide is where models read screen
// text reliably without spending tokens on pixels they cannot use.
const (
	agentMaxWidth  = 1280
	agentMaxHeight = 800
	agentQuality   = 80
)

// DesktopAgentFile is where the agent's URL and token are written, in the
// data directory.
const DesktopAgentFile = "desktop-agent.json"

// DesktopAgent is that file's contents.
type DesktopAgent struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// StartDesktop opens the display and writes the agent's file. Called once at
// startup when --desktop is set.
func (s *Server) StartDesktop() error {
	if s.Cfg.Desktop == "" {
		return nil
	}
	s.Desktop = desktop.Open(s.Cfg.Desktop)
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	s.desktopToken = hex.EncodeToString(b[:])
	s.Desktop.OnAction(func(a desktop.Action) {
		if a.By != desktop.ByAgent {
			return
		}
		detail := a.Kind
		if a.Kind != "type" && a.Kind != "key" {
			detail += fmt.Sprintf(" %d,%d", a.X, a.Y)
		}
		if a.Text != "" {
			detail += " " + strconv.Quote(a.Text)
		}
		s.audit(context.Background(), "desktop.agent", "agent", "local", detail)
	})
	file := DesktopAgent{URL: localURL(s.Cfg), Token: s.desktopToken}
	data, _ := json.Marshal(file)
	path := filepath.Join(s.Cfg.DataDir, DesktopAgentFile)
	// Written fresh on every start, mode 0600 before any byte lands: the
	// token is the agent's whole capability.
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// localURL is how a process on this machine reaches the panel.
func localURL(cfg config.Config) string {
	scheme := "http"
	if cfg.TLSMode != config.TLSOff {
		scheme = "https"
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || port == "" {
		port = strconv.Itoa(config.DefaultPort)
	}
	return scheme + "://127.0.0.1:" + port
}

func (s *Server) registerDesktopRoutes(r chi.Router) {
	r.Get("/desktop", s.handleDesktopStatus)
	r.Post("/desktop/stop", s.handleDesktopStop)
	r.Post("/desktop/resume", s.handleDesktopResume)
	r.Post("/desktop/input", s.handleDesktopInput)
	r.Get("/desktop/stream", s.handleDesktopStream)
}

func (s *Server) registerDesktopAgentRoutes(r chi.Router) {
	r.Get("/desktop/agent/screenshot", s.requireDesktopAgent(s.handleAgentScreenshot))
	r.Post("/desktop/agent/act", s.requireDesktopAgent(s.handleAgentAct))
}

func (s *Server) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	if s.Desktop == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "status": s.Desktop.Status()})
}

func (s *Server) desktopOff(w http.ResponseWriter) bool {
	if s.Desktop == nil {
		writeErr(w, http.StatusNotFound, "the desktop is off; start the panel with --desktop :0")
		return true
	}
	return false
}

func (s *Server) handleDesktopStop(w http.ResponseWriter, r *http.Request) {
	if s.desktopOff(w) {
		return
	}
	s.Desktop.Stop()
	if u, ok := currentUserFrom(r); ok {
		s.audit(r.Context(), "desktop.stop", u.Username, s.clientIP(r), "")
	}
	writeJSON(w, http.StatusOK, s.Desktop.Status())
}

func (s *Server) handleDesktopResume(w http.ResponseWriter, r *http.Request) {
	if s.desktopOff(w) {
		return
	}
	s.Desktop.Resume()
	if u, ok := currentUserFrom(r); ok {
		s.audit(r.Context(), "desktop.resume", u.Username, s.clientIP(r), "")
	}
	writeJSON(w, http.StatusOK, s.Desktop.Status())
}

// desktopInput is one thing the person did on the view, in screen pixels.
type desktopInput struct {
	Type   string `json:"type"` // move, down, up, click, scroll, keydown, keyup, type
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Button string `json:"button"`
	Count  int    `json:"count"`
	DX     int    `json:"dx"`
	DY     int    `json:"dy"`
	Key    string `json:"key"`
	Text   string `json:"text"`
}

func (s *Server) handleDesktopInput(w http.ResponseWriter, r *http.Request) {
	if s.desktopOff(w) {
		return
	}
	var in desktopInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad input: "+err.Error())
		return
	}
	if err := s.applyInput(desktop.ByPerson, in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applyInput(by desktop.Source, in desktopInput) error {
	d := s.Desktop
	switch in.Type {
	case "move":
		return d.Move(by, in.X, in.Y)
	case "down", "up":
		b, err := desktop.ButtonByName(in.Button)
		if err != nil {
			return err
		}
		return d.Press(by, in.X, in.Y, b, in.Type == "down")
	case "click":
		b, err := desktop.ButtonByName(in.Button)
		if err != nil {
			return err
		}
		return d.Click(by, in.X, in.Y, b, max(1, in.Count))
	case "scroll":
		return d.Scroll(by, in.X, in.Y, in.DX, in.DY)
	case "keydown", "keyup":
		sym, ok := desktop.KeysymByName(in.Key)
		if !ok {
			return fmt.Errorf("no key called %q", in.Key)
		}
		return d.KeyEvent(by, sym, in.Type == "keydown")
	case "type":
		return d.Type(by, in.Text)
	}
	return fmt.Errorf("no input called %q", in.Type)
}

// handleDesktopStream is the live view: binary messages of records (see
// internal/desktop/stream.go -- a keyframe header, JPEG tiles, the pointer),
// and a JSON status message once a second and whenever it changes.
func (s *Server) handleDesktopStream(w http.ResponseWriter, r *http.Request) {
	if s.desktopOff(w) {
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	// Same origin rule as the terminal's socket: no OriginPatterns, so only a
	// page on this panel's own origin can open it with the cookie.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The tiles are JPEG already; deflating them again is CPU spent on
		// the machine that has least of it, for nothing.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Reads only to notice the browser going away.
	go func() {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				cancel()
				return
			}
		}
	}()

	frames, stop := s.Desktop.Watch(width)
	defer stop()
	statusTick := time.NewTicker(time.Second)
	defer statusTick.Stop()
	authTick := time.NewTicker(10 * time.Second)
	defer authTick.Stop()
	var lastStatus []byte
	sendStatus := func() error {
		b, _ := json.Marshal(s.Desktop.Status())
		if string(b) == string(lastStatus) {
			return nil
		}
		lastStatus = b
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	if err := sendStatus(); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-authTick.C:
			// A view is authorised at the handshake and can live for hours;
			// signing out has to end it, as it ends the terminal's.
			if !s.stillAuthorized(r) {
				c.Close(websocket.StatusPolicyViolation, "signed out")
				return
			}
		case <-statusTick.C:
			if err := sendStatus(); err != nil {
				return
			}
		case msg := <-frames:
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Write(wctx, websocket.MessageBinary, msg)
			wcancel()
			if err != nil {
				return
			}
			// A status change usually comes with a frame (the agent acted);
			// sending it now keeps the "agent clicked here" marker with the
			// picture it belongs to.
			if err := sendStatus(); err != nil {
				return
			}
		}
	}
}

// requireDesktopAgent admits the agent's token and nothing else.
func (s *Server) requireDesktopAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Desktop == nil || s.desktopToken == "" {
			writeErr(w, http.StatusNotFound, "the desktop is off")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.desktopToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "not the desktop agent's token")
			return
		}
		next(w, r)
	}
}

// agentShot is a screenshot as the agent receives it.
type agentShot struct {
	Image        string `json:"image"` // base64 JPEG
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ScreenWidth  int    `json:"screenWidth"`
	ScreenHeight int    `json:"screenHeight"`
	PointerX     int    `json:"pointerX"`
	PointerY     int    `json:"pointerY"`
	Stopped      bool   `json:"stopped"`
}

func (s *Server) agentScreenshot() (agentShot, error) {
	img, sw, sh, err := s.Desktop.Shot(agentMaxWidth, agentMaxHeight)
	if err != nil {
		return agentShot{}, err
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	b, err := desktop.JPEG(img, agentQuality)
	if err != nil {
		return agentShot{}, err
	}
	px, py, _ := s.Desktop.Pointer()
	st := s.Desktop.Status()
	return agentShot{
		Image: base64.StdEncoding.EncodeToString(b), Width: w, Height: h,
		ScreenWidth: sw, ScreenHeight: sh,
		PointerX: px * w / sw, PointerY: py * h / sh,
		Stopped: st.Stopped,
	}, nil
}

func (s *Server) handleAgentScreenshot(w http.ResponseWriter, r *http.Request) {
	shot, err := s.agentScreenshot()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shot)
}

// agentAct is one action, in the coordinates of the agent's screenshots.
type agentAct struct {
	Action  string  `json:"action"` // move, click, double_click, right_click, drag, scroll, type, key, wait
	X       int     `json:"x"`
	Y       int     `json:"y"`
	X2      int     `json:"x2"`
	Y2      int     `json:"y2"`
	Text    string  `json:"text"`
	Key     string  `json:"key"`
	DX      int     `json:"dx"`
	DY      int     `json:"dy"`
	Seconds float64 `json:"seconds"`
	// Screenshot after acting, true unless set false: an agent nearly always
	// wants to see what its click did, and one round trip is cheaper than two.
	Screenshot *bool `json:"screenshot"`
}

func (s *Server) handleAgentAct(w http.ResponseWriter, r *http.Request) {
	var a agentAct
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&a); err != nil {
		writeErr(w, http.StatusBadRequest, "bad action: "+err.Error())
		return
	}
	sw, sh, err := s.Desktop.Size()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	aw, ah := desktop.FitWidth(sw, sh, agentMaxWidth, agentMaxHeight)
	// From the agent's screenshot to the screen, rounding to the pixel the
	// agent's pixel covers the middle of.
	sx := func(x int) int { return (2*x + 1) * sw / (2 * aw) }
	sy := func(y int) int { return (2*y + 1) * sh / (2 * ah) }
	d := s.Desktop
	by := desktop.ByAgent
	switch a.Action {
	case "move":
		err = d.Move(by, sx(a.X), sy(a.Y))
	case "click", "left_click":
		err = d.Click(by, sx(a.X), sy(a.Y), desktop.ButtonLeft, 1)
	case "double_click":
		err = d.Click(by, sx(a.X), sy(a.Y), desktop.ButtonLeft, 2)
	case "triple_click":
		err = d.Click(by, sx(a.X), sy(a.Y), desktop.ButtonLeft, 3)
	case "right_click":
		err = d.Click(by, sx(a.X), sy(a.Y), desktop.ButtonRight, 1)
	case "middle_click":
		err = d.Click(by, sx(a.X), sy(a.Y), desktop.ButtonMiddle, 1)
	case "drag":
		err = d.Drag(by, sx(a.X), sy(a.Y), sx(a.X2), sy(a.Y2))
	case "scroll":
		err = d.Scroll(by, sx(a.X), sy(a.Y), a.DX, a.DY)
	case "type":
		err = d.Type(by, a.Text)
	case "key":
		err = d.Key(by, a.Key)
	case "wait":
		secs := min(max(a.Seconds, 0), 10)
		time.Sleep(time.Duration(secs * float64(time.Second)))
	default:
		writeErr(w, http.StatusBadRequest, "no action called "+strconv.Quote(a.Action))
		return
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, desktop.ErrStopped) || errors.Is(err, desktop.ErrPersonActive) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	res := map[string]any{"ok": true}
	if a.Screenshot == nil || *a.Screenshot {
		// Long enough for a menu to open or a page to start drawing; a slow
		// page the agent can wait for itself.
		time.Sleep(350 * time.Millisecond)
		if shot, serr := s.agentScreenshot(); serr == nil {
			res["screenshot"] = shot
		}
	}
	writeJSON(w, http.StatusOK, res)
}
