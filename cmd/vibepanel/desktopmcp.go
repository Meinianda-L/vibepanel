package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jiangmuran/vibepanel/internal/config"
	"github.com/jiangmuran/vibepanel/internal/httpapi"
	"github.com/jiangmuran/vibepanel/internal/version"
)

// `vibepanel desktop-mcp` gives an agent the panel's desktop: screenshots,
// the pointer and the keyboard of the display the panel was started with
// --desktop. Added to an agent once:
//
//	claude mcp add desktop -- vibepanel desktop-mcp
//
// Every tool is one request to the panel's /api/desktop/agent/* with the
// token the panel wrote to desktop-agent.json at start, rather than this
// process talking to X itself, for one reason: the person at the panel can
// press Stop, and Stop has to be something the panel enforces, not something
// it asks this process to honour.
func cmdDesktopMCP(args []string) error {
	fs := flag.NewFlagSet("desktop-mcp", flag.ContinueOnError)
	file := fs.String("file", "", "the panel's desktop-agent.json (default: in the data directory)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := desktopClientFromFile(*file)
	if err != nil {
		return err
	}
	return serveRPC(os.Stdin, os.Stdout, os.Stderr, "vibepanel desktop-mcp", func(ctx context.Context, req rpcRequest) (any, *rpcError) {
		return handleDesktopMCP(ctx, c, req)
	})
}

func defaultDesktopAgentFile() string {
	return filepath.Join(config.Default().DataDir, httpapi.DesktopAgentFile)
}

func newDesktopClient(panelURL, token string) (*desktopClient, error) {
	u, err := url.Parse(panelURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("vibepanel desktop: %q is not a URL", panelURL)
	}
	return &desktopClient{
		base: strings.TrimRight(panelURL, "/"), token: token,
		http: &http.Client{Timeout: 60 * time.Second, Transport: mcpTransport(u.Hostname())},
	}, nil
}

type desktopClient struct {
	base  string
	token string
	http  *http.Client
}

// desktopShot is what the panel returns for a screenshot.
type desktopShot struct {
	Image        string `json:"image"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	ScreenWidth  int    `json:"screenWidth"`
	ScreenHeight int    `json:"screenHeight"`
	PointerX     int    `json:"pointerX"`
	PointerY     int    `json:"pointerY"`
	Stopped      bool   `json:"stopped"`
}

func (c *desktopClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/desktop/agent"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("panel answered %s", resp.Status)
	}
	return json.Unmarshal(data, out)
}

func point(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func xyTool(name, desc string) mcpTool {
	return mcpTool{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"x": point("Horizontal position in the latest screenshot's pixels."),
				"y": point("Vertical position in the latest screenshot's pixels."),
			},
			"required":             []string{"x", "y"},
			"additionalProperties": false,
		},
	}
}

const desktopToolNote = " Coordinates are pixels of the screenshots this server returns, not of the physical screen. Returns a screenshot taken just after."

var desktopTools = []mcpTool{
	{
		Name: "screenshot",
		Description: "See the desktop: a screenshot of the whole screen, at most 1280x800, and where the pointer is. " +
			"Take one before acting, and look at the one each action returns before the next.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
	},
	xyTool("click", "Left-click at x, y."+desktopToolNote),
	xyTool("double_click", "Double-click at x, y."+desktopToolNote),
	xyTool("triple_click", "Triple-click at x, y, which selects a line in most text."+desktopToolNote),
	xyTool("right_click", "Right-click at x, y, for a context menu."+desktopToolNote),
	xyTool("move", "Move the pointer to x, y without clicking, for hover menus and tooltips."+desktopToolNote),
	{
		Name:        "drag",
		Description: "Press the left button at x, y, move to x2, y2, release." + desktopToolNote,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"x": point("Start, horizontal."), "y": point("Start, vertical."),
				"x2": point("End, horizontal."), "y2": point("End, vertical."),
			},
			"required":             []string{"x", "y", "x2", "y2"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "scroll",
		Description: "Turn the mouse wheel at x, y. dy is notches down (negative is up), dx is notches right (negative is left); three notches is about a screen of text." + desktopToolNote,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"x": point("Where to scroll, horizontal."), "y": point("Where to scroll, vertical."),
				"dy": point("Notches down; negative scrolls up."),
				"dx": point("Notches right; negative scrolls left."),
			},
			"required":             []string{"x", "y"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "type",
		Description: "Type text into whatever has keyboard focus, as keystrokes. Any language, including Chinese. Click the field first. Use key for Enter, Tab and shortcuts." + " Returns a screenshot taken just after.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"text": map[string]any{"type": "string", "description": "The text, up to 4000 characters."}},
			"required":             []string{"text"},
			"additionalProperties": false,
		},
	},
	{
		Name: "key",
		Description: "Press a key or a combination: Return, Escape, Tab, BackSpace, Delete, Up, Down, Left, Right, Home, End, Page_Up, Page_Down, F1-F12, " +
			"or modifiers joined with +: ctrl+l, ctrl+shift+t, alt+F4, super. Returns a screenshot taken just after.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"key": map[string]any{"type": "string", "description": "The key or combination."}},
			"required":             []string{"key"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "wait",
		Description: "Wait for something to load, up to 10 seconds, then take a screenshot.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"seconds": map[string]any{"type": "number", "description": "How long; 1 by default."}},
			"additionalProperties": false,
		},
	},
}

func handleDesktopMCP(ctx context.Context, c *desktopClient, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		proto := mcpDefaultProtocol
		for _, v := range mcpProtocolVersions {
			if p.ProtocolVersion == v {
				proto = v
			}
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vibepanel-desktop", "version": version.String()},
			"instructions": "These tools operate a real desktop that a person can watch live in vibepanel and can stop at any moment. " +
				"If an action is refused because the person stopped you or is using the screen, stop acting and tell them; do not retry in a loop.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": desktopTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "tools/call needs a name"}
		}
		return callDesktopTool(ctx, c, p.Name, p.Arguments)
	}
	return nil, &rpcError{Code: rpcMethodNotFound, Message: "method not found: " + req.Method}
}

func callDesktopTool(ctx context.Context, c *desktopClient, name string, raw json.RawMessage) (any, *rpcError) {
	if name == "screenshot" {
		var s desktopShot
		if err := c.do(ctx, http.MethodGet, "/screenshot", nil, &s); err != nil {
			return toolResult(err.Error(), true), nil
		}
		return shotResult("", s), nil
	}
	known := false
	for _, t := range desktopTools {
		known = known || t.Name == name
	}
	if !known {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "unknown tool: " + name}
	}
	act := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &act); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "arguments: " + err.Error()}
		}
	}
	if name == "wait" {
		if _, ok := act["seconds"]; !ok {
			act["seconds"] = 1
		}
	}
	act["action"] = name
	var res struct {
		OK         bool         `json:"ok"`
		Screenshot *desktopShot `json:"screenshot"`
	}
	if err := c.do(ctx, http.MethodPost, "/act", act, &res); err != nil {
		return toolResult(err.Error(), true), nil
	}
	if res.Screenshot == nil {
		return toolResult("done", false), nil
	}
	return shotResult("done. ", *res.Screenshot), nil
}

// shotResult is a screenshot as MCP content: the picture, and a line saying
// what space its coordinates are in.
func shotResult(prefix string, s desktopShot) map[string]any {
	text := fmt.Sprintf("%sScreenshot %dx%d (the screen is %dx%d; use the screenshot's coordinates). Pointer at %d,%d.",
		prefix, s.Width, s.Height, s.ScreenWidth, s.ScreenHeight, s.PointerX, s.PointerY)
	if s.Stopped {
		text += " The person at the panel has stopped you: do not act until they resume."
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "image", "data": s.Image, "mimeType": "image/jpeg"},
			{"type": "text", "text": text},
		},
	}
}
