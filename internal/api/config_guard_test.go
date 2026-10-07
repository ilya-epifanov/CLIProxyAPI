package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const configGuardTestKey = "test-management-key"

// newConfigGuardTestServer starts a server whose config file exists with the given mode.
func newConfigGuardTestServer(t *testing.T, mode os.FileMode) (*Server, string) {
	t.Helper()
	if mode&0o200 == 0 && os.Geteuid() == 0 {
		t.Skip("root can write files regardless of mode bits")
	}
	t.Setenv("MANAGEMENT_PASSWORD", configGuardTestKey)
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("config-version: 8\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	if errChmod := os.Chmod(configPath, mode); errChmod != nil {
		t.Fatalf("chmod config: %v", errChmod)
	}
	cfg := &proxyconfig.Config{AuthDir: filepath.Join(tmpDir, "auth")}
	return NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), configPath), configPath
}

func serveManagement(server *Server, method, path, body string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+configGuardTestKey)
	}
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	return rr
}

type capabilitiesResponse struct {
	ConfigWritable bool     `json:"config_writable"`
	Strategies     []string `json:"strategies"`
}

func getCapabilities(t *testing.T, server *Server) capabilitiesResponse {
	t.Helper()
	rr := serveManagement(server, http.MethodGet, "/v8/management/server/capabilities", "", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got capabilitiesResponse
	if errDecode := json.Unmarshal(rr.Body.Bytes(), &got); errDecode != nil {
		t.Fatalf("decode capabilities: %v body=%s", errDecode, rr.Body.String())
	}
	return got
}

func TestConfigWriteGuardRefusesConfigWritesWhenReadOnly(t *testing.T) {
	server, _ := newConfigGuardTestServer(t, 0o400)

	caps := getCapabilities(t, server)
	if caps.ConfigWritable {
		t.Fatal("config_writable = true for a read-only config")
	}
	if !slices.Contains(caps.Strategies, "round-robin") {
		t.Fatalf("strategies = %v, want round-robin among them", caps.Strategies)
	}

	refused := []struct{ method, path, body string }{
		{http.MethodPut, "/v8/management/config.yaml", "config-version: 8\n"},
		{http.MethodPut, "/v8/management/config", `{"config-version":8}`},
		{http.MethodPatch, "/v8/management/config", `{"routing":{"strategy":"fill-first"}}`},
		{http.MethodPut, "/v8/management/config/routing/strategy", `{"value":"fill-first"}`},
		{http.MethodDelete, "/v8/management/config/requests/proxy-url", ""},
		{http.MethodDelete, "/v8/management/plugins/example", ""},
		{http.MethodPost, "/v8/management/plugins/store/example/install", "{}"},
	}
	for _, req := range refused {
		rr := serveManagement(server, req.method, req.path, req.body, true)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"config_read_only"`) {
			t.Errorf("%s %s: status = %d body=%s, want 409 config_read_only", req.method, req.path, rr.Code, rr.Body.String())
		}
	}

	if rr := serveManagement(server, http.MethodPut, "/v8/management/config.yaml", "config-version: 8\n", false); rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated write: status = %d, want 401 before the guard runs", rr.Code)
	}
	if rr := serveManagement(server, http.MethodPost, "/v8/management/routing/cooldown/reset", "{}", true); rr.Code == http.StatusConflict {
		t.Errorf("exempt route refused: body=%s", rr.Body.String())
	}
	if rr := serveManagement(server, http.MethodGet, "/v8/management/config", "", true); rr.Code != http.StatusOK {
		t.Errorf("read: status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestConfigWriteGuardFollowsFileMode(t *testing.T) {
	server, configPath := newConfigGuardTestServer(t, 0o600)

	if !getCapabilities(t, server).ConfigWritable {
		t.Fatal("config_writable = false for a writable config")
	}
	if rr := serveManagement(server, http.MethodPut, "/v8/management/config.yaml", "config-version: 8\n", true); rr.Code != http.StatusOK {
		t.Fatalf("write to writable config: status = %d body=%s", rr.Code, rr.Body.String())
	}

	if os.Geteuid() == 0 {
		return
	}
	if errChmod := os.Chmod(configPath, 0o400); errChmod != nil {
		t.Fatalf("chmod config: %v", errChmod)
	}
	if getCapabilities(t, server).ConfigWritable {
		t.Fatal("config_writable stayed true after chmod 0400")
	}
}

// Every exempt route must exist, so an upstream rename cannot leave a stale entry behind.
func TestNonConfigMutationRoutesAreRegistered(t *testing.T) {
	server, _ := newConfigGuardTestServer(t, 0o600)

	registered := make(map[string]struct{})
	for _, route := range server.engine.Routes() {
		registered[route.Method+" "+route.Path] = struct{}{}
	}
	for _, route := range management.NonConfigMutationRoutes() {
		if _, ok := registered[route]; !ok {
			t.Errorf("exempt route %q is not registered", route)
		}
	}
}
