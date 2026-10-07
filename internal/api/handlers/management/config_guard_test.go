package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// writeConfigFile writes a minimal config with the given mode. Root ignores mode bits, so read-only cases skip there.
func writeConfigFile(t *testing.T, mode os.FileMode) string {
	t.Helper()
	if mode&0o200 == 0 && os.Geteuid() == 0 {
		t.Skip("root can write files regardless of mode bits")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte("config-version: 8\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	if errChmod := os.Chmod(path, mode); errChmod != nil {
		t.Fatalf("chmod config: %v", errChmod)
	}
	return path
}

func TestConfigWritable(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		want bool
	}{
		{name: "writable file", path: func(t *testing.T) string { return writeConfigFile(t, 0o600) }, want: true},
		{name: "read-only file", path: func(t *testing.T) string { return writeConfigFile(t, 0o400) }, want: false},
		{name: "missing file", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.yaml") }, want: false},
		{name: "no config path", path: func(*testing.T) string { return "" }, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHandler(&config.Config{}, tt.path(t), nil)
			if got := h.ConfigWritable(); got != tt.want {
				t.Fatalf("ConfigWritable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoutingStrategiesAreCanonical(t *testing.T) {
	for _, strategy := range routingStrategies {
		if got, ok := normalizeRoutingStrategy(strategy); !ok || got != strategy {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want %q, true", strategy, got, ok, strategy)
		}
	}
}

func TestPatchAuthFileStatusRefusesConfigAPIKeyToggleWhenConfigReadOnly(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	cfg := &config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:  "sk-test",
			BaseURL: "https://example.com/v1",
		}},
	}
	authID, _ := synthesizer.NewStableIDGenerator().Next("codex:apikey", "sk-test", "https://example.com/v1", "", "", "")
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://example.com/v1",
			"source":   "config:codex[abc]",
		},
	}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandler(cfg, writeConfigFile(t, 0o400), manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/status", strings.NewReader(`{"name":"`+authID+`","disabled":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(ConfigV8ContextKey, true)

	h.PatchAuthFileStatus(ctx)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil || body.Error != errConfigReadOnly {
		t.Fatalf("body = %s, want error %q", rec.Body.String(), errConfigReadOnly)
	}
	if excluded := cfg.CodexKey[0].ExcludedModels; len(excluded) != 0 {
		t.Fatalf("in-memory excluded-models changed to %#v", excluded)
	}
}
