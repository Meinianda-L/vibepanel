package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jiangmuran/vibepanel/internal/desktop"
)

// TestTheDesktopAgentsTokenOpensItsTwoRoutesAndNothingElse pins the shape
// desktop.go describes: the agent's token is not a session, and a session is
// not the agent's token. Either crossing over would be a way for one surface
// to act as the other -- a page riding the person's cookie as the agent, or
// the agent's token reaching the terminal.
func TestTheDesktopAgentsTokenOpensItsTwoRoutesAndNothingElse(t *testing.T) {
	ts, srv := newTestServer(t)
	// A display that does not exist: reaching the handler shows as 503 (it
	// cannot capture), refusing at the door shows as 401.
	srv.Desktop = desktop.Open(":4321")
	srv.desktopToken = "the-agents-token"

	do := func(c *http.Client, method, path, bearer, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	signedIn := ts.Client()
	stranger := &http.Client{}

	for _, r := range []struct{ method, path, body string }{
		{"GET", "/api/desktop/agent/screenshot", ""},
		{"POST", "/api/desktop/agent/act", `{"action":"wait","seconds":0}`},
	} {
		if got := do(signedIn, r.method, r.path, "", r.body); got != http.StatusUnauthorized {
			t.Errorf("%s %s with the person's session and no token: %d, want 401", r.method, r.path, got)
		}
		if got := do(stranger, r.method, r.path, "", r.body); got != http.StatusUnauthorized {
			t.Errorf("%s %s with nothing: %d, want 401", r.method, r.path, got)
		}
		if got := do(stranger, r.method, r.path, "not-the-token", r.body); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token: %d, want 401", r.method, r.path, got)
		}
		if got := do(stranger, r.method, r.path, "the-agents-token", r.body); got == http.StatusUnauthorized {
			t.Errorf("%s %s with the agent's token was refused", r.method, r.path)
		}
	}
	// The agent's token is an unknown string to everything else.
	for _, path := range []string{"/api/state", "/api/desktop", "/api/settings/audit"} {
		if got := do(stranger, "GET", path, "the-agents-token", ""); got != http.StatusUnauthorized {
			t.Errorf("GET %s with the agent's token: %d, want 401", path, got)
		}
	}
	if got := do(stranger, "POST", "/api/desktop/stop", "the-agents-token", ""); got != http.StatusUnauthorized {
		t.Errorf("the agent's token pressed Stop on itself: %d, want 401", got)
	}
	// And the person's routes are the person's.
	if got := do(signedIn, "GET", "/api/desktop", "", ""); got != http.StatusOK {
		t.Errorf("GET /api/desktop signed in: %d", got)
	}
}

// TestStopIsTheDesktopsAndTheAgentIsRefusedUntilResume checks the order of
// the controls through the API: Stop is audited, the agent is told why it
// was refused, Resume undoes it.
func TestStopIsTheDesktopsAndTheAgentIsRefusedUntilResume(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.Desktop = desktop.Open(":4321")
	srv.desktopToken = "tok"
	res, err := ts.Client().Post(ts.URL+"/api/desktop/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !srv.Desktop.Status().Stopped {
		t.Fatalf("stop: %d, stopped=%v", res.StatusCode, srv.Desktop.Status().Stopped)
	}
	if !auditHas(t, srv, "desktop.stop", "") {
		t.Error("Stop was not audited")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/desktop/agent/act", strings.NewReader(`{"action":"click","x":1,"y":1}`))
	req.Header.Set("Authorization", "Bearer tok")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	// Refused before it reaches the display, which here does not exist.
	if res.StatusCode != http.StatusConflict && res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("an agent click while stopped: %d", res.StatusCode)
	}
	res, err = ts.Client().Post(ts.URL+"/api/desktop/resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if srv.Desktop.Status().Stopped {
		t.Error("still stopped after Resume")
	}
}
