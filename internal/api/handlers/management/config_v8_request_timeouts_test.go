package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The management API is how the KeyManageCenter panel reads and writes request
// timeouts, so the requests.timeouts subtree must round-trip through the v8
// config endpoints without losing rules or clamping values.
func TestConfigV8RequestTimeoutsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "server: {port: 8317}\nrequests: {proxy-url: \"\"}\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	reloads := make(chan *config.Config, 8)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloads <- cfg })
	router := gin.New()
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	request := func(method, url, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, url, strings.NewReader(body)))
		if recorder.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, url, recorder.Code, recorder.Body.String())
		}
		return recorder
	}

	request(http.MethodPut, "/v8/management/config/requests/timeouts", `{
  "stream-first-byte-seconds": 90,
  "stream-idle-seconds": 0,
  "nonstream-seconds": 1200,
  "rules": [{"models": ["claude-*"], "providers": ["claude"], "nonstream-seconds": 300}]
}`, http.StatusOK)

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "requests:") {
		t.Fatalf("timeouts were not written under requests: %s", saved)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	global := loaded.Timeouts.Resolve("gpt-5", []string{"openai"}, []string{"codex"})
	if global.StreamFirstByte != 90*time.Second || global.StreamIdle != 0 || global.NonStream != 1200*time.Second {
		t.Fatalf("global timeouts = %+v, want the configured values", global)
	}
	claude := loaded.Timeouts.Resolve("claude-sonnet-4-5", []string{"claude"}, []string{"claude"})
	if claude.NonStream != 300*time.Second {
		t.Fatalf("rule timeout = %s, want the rule to override the global", claude.NonStream)
	}
	if claude.StreamFirstByte != 90*time.Second {
		t.Fatalf("rule StreamFirstByte = %s, want the global fallback", claude.StreamFirstByte)
	}

	select {
	case reloaded := <-reloads:
		if reloaded == nil || reloaded.Timeouts.Resolve("claude-sonnet-4-5", nil, []string{"claude"}).NonStream != 300*time.Second {
			t.Fatal("reload hook did not carry the new timeouts")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("config reload hook was not called")
	}

	// Values outside the supported ranges are clamped rather than rejected.
	request(http.MethodPut, "/v8/management/config/requests/timeouts", `{
  "stream-first-byte-seconds": 999,
  "stream-idle-seconds": 5,
  "nonstream-seconds": 1
}`, http.StatusOK)
	loaded, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	clamped := loaded.Timeouts.Resolve("", nil, nil)
	if clamped.StreamFirstByte != 120*time.Second || clamped.StreamIdle != 60*time.Second || clamped.NonStream != 60*time.Second {
		t.Fatalf("clamped timeouts = %+v, want documented bounds", clamped)
	}

	// Invalid shapes are rejected without touching the file.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	request(http.MethodPut, "/v8/management/config/requests/timeouts", `{"stream-idle-seconds": "soon"}`, http.StatusUnprocessableEntity)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a rejected write modified the config file")
	}

	// The GET path exposes the same subtree for read-modify-write UIs.
	body := request(http.MethodGet, "/v8/management/config/requests/timeouts", "", http.StatusOK).Body.String()
	if !strings.Contains(body, "stream-first-byte-seconds") || !strings.Contains(body, "nonstream-seconds") {
		t.Fatalf("GET response = %q, want the timeouts subtree", body)
	}
}
