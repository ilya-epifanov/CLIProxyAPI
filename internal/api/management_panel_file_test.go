package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagementPanelRevalidatesByContent(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	panelPath := filepath.Join(staticDir, "management.html")
	// Every Nix build of the panel has this modification time.
	epoch := time.Unix(1, 0)
	writePanel := func(body string) {
		t.Helper()
		if err := os.WriteFile(panelPath, []byte(body), 0o600); err != nil {
			t.Fatalf("write panel: %v", err)
		}
		if err := os.Chtimes(panelPath, epoch, epoch); err != nil {
			t.Fatalf("set panel time: %v", err)
		}
	}
	server := newTestServer(t)
	get := func(header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/management.html", nil)
		for name, value := range header {
			req.Header.Set(name, value)
		}
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		return rr
	}

	writePanel("<html>panel v1</html>")
	first := get(nil)
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || first.Body.String() != "<html>panel v1</html>" {
		t.Fatalf("first load: status %d body %q", first.Code, first.Body.String())
	}
	if etag == "" || first.Header().Get("Cache-Control") != "no-cache" || first.Header().Get("Last-Modified") != "" {
		t.Fatalf("first load headers = %v, want an ETag, no-cache and no Last-Modified", first.Header())
	}
	if !strings.HasPrefix(first.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q", first.Header().Get("Content-Type"))
	}

	if unchanged := get(map[string]string{"If-None-Match": etag}); unchanged.Code != http.StatusNotModified {
		t.Fatalf("revalidating an unchanged panel: status %d, want 304", unchanged.Code)
	}

	writePanel("<html>panel v2</html>")
	revalidated := get(map[string]string{
		"If-None-Match":     etag,
		"If-Modified-Since": epoch.UTC().Format(http.TimeFormat),
	})
	if revalidated.Code != http.StatusOK || revalidated.Body.String() != "<html>panel v2</html>" {
		t.Fatalf("revalidating a replaced panel with the same time: status %d body %q", revalidated.Code, revalidated.Body.String())
	}
	if revalidated.Header().Get("ETag") == etag {
		t.Fatal("a replaced panel kept its ETag")
	}
}
