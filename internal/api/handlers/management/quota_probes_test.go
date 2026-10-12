package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaprobe"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeQuotaProbes struct {
	entries   []quotaprobe.Entry
	refreshed string
	err       error
}

func (f *fakeQuotaProbes) Entries() []quotaprobe.Entry { return f.entries }

func (f *fakeQuotaProbes) Refresh(_ context.Context, authIndex string) (quotaprobe.Entry, error) {
	f.refreshed = authIndex
	if f.err != nil {
		return quotaprobe.Entry{}, f.err
	}
	return f.entries[0], nil
}

func serveQuotaProbes(t *testing.T, handler func(*gin.Context), method, target, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

func TestQuotaProbeEndpointsWithoutProber(t *testing.T) {
	h := &Handler{}
	if code, body := serveQuotaProbes(t, h.GetQuotaProbes, http.MethodGet, "/", ""); code != http.StatusNotFound || body["error"] != "quota_probes_disabled" {
		t.Fatalf("GET: %d %v", code, body)
	}
	if code, _ := serveQuotaProbes(t, h.RefreshQuotaProbe, http.MethodPost, "/", `{"auth_index":"a1"}`); code != http.StatusNotFound {
		t.Fatalf("POST: %d", code)
	}
}

func TestQuotaProbeEndpoints(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	probes := &fakeQuotaProbes{entries: []quotaprobe.Entry{
		{AuthIndex: "a1", AttemptedAt: fetchedAt, FetchedAt: &fetchedAt,
			Responses: map[string]quotaprobe.Response{"usage": {StatusCode: 200, Body: `{"five_hour":{}}`}}},
		{AuthIndex: "c1", AttemptedAt: fetchedAt, Error: &quotaprobe.Failure{Status: 401, Message: "expired"},
			Responses: map[string]quotaprobe.Response{}},
	}}
	h := &Handler{}
	h.SetQuotaProbes(probes)

	code, body := serveQuotaProbes(t, h.GetQuotaProbes, http.MethodGet, "/", "")
	entries, _ := body["entries"].([]any)
	if code != http.StatusOK || len(entries) != 2 {
		t.Fatalf("GET: %d %v", code, body)
	}
	entry := entries[1].(map[string]any)
	if entry["fetched_at"] != nil || entry["error"].(map[string]any)["status"] != float64(401) {
		t.Fatalf("entry = %v", entry)
	}

	code, body = serveQuotaProbes(t, h.RefreshQuotaProbe, http.MethodPost, "/", `{"auth_index":"a1"}`)
	if code != http.StatusOK || probes.refreshed != "a1" || body["fetched_at"] != "2026-10-11T10:00:00Z" ||
		body["responses"].(map[string]any)["usage"].(map[string]any)["body"] != `{"five_hour":{}}` {
		t.Fatalf("POST: %d %v", code, body)
	}

	if code, _ = serveQuotaProbes(t, h.RefreshQuotaProbe, http.MethodPost, "/", `{}`); code != http.StatusBadRequest {
		t.Fatalf("POST without auth_index: %d", code)
	}
	probes.err = quotaprobe.ErrUnknownAuth
	if code, body = serveQuotaProbes(t, h.RefreshQuotaProbe, http.MethodPost, "/", `{"auth_index":"zz"}`); code != http.StatusNotFound || body["error"] != "credential_not_found" {
		t.Fatalf("POST unknown: %d %v", code, body)
	}
	probes.err = quotaprobe.ErrUnsupportedProvider
	if code, _ = serveQuotaProbes(t, h.RefreshQuotaProbe, http.MethodPost, "/", `{"auth_index":"g1"}`); code != http.StatusBadRequest {
		t.Fatalf("POST unsupported: %d", code)
	}
}

func TestDoQuotaProbeSendsTheAccessToken(t *testing.T) {
	var gotAuth, gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer upstream.Close()

	h := &Handler{}
	auth := &coreauth.Auth{Provider: "claude", Metadata: map[string]any{"access_token": "tok-123"}}
	response, err := h.DoQuotaProbe(context.Background(), auth, quotaprobe.Request{
		Name:   "usage",
		URL:    upstream.URL,
		Header: map[string]string{"anthropic-beta": "oauth-2025-04-20"},
	})
	if err != nil {
		t.Fatalf("DoQuotaProbe: %v", err)
	}
	if gotAuth != "Bearer tok-123" || gotBeta != "oauth-2025-04-20" {
		t.Fatalf("upstream saw Authorization %q, anthropic-beta %q", gotAuth, gotBeta)
	}
	if response.StatusCode != http.StatusTooManyRequests || response.Body != `{"error":"slow down"}` {
		t.Fatalf("response = %+v", response)
	}

	if _, err = h.DoQuotaProbe(context.Background(), &coreauth.Auth{Provider: "claude"}, quotaprobe.Request{URL: upstream.URL}); err == nil {
		t.Fatal("DoQuotaProbe without a token succeeded")
	}
}

func TestCapabilitiesReportQuotaCache(t *testing.T) {
	h := &Handler{}
	h.SetQuotaProbes(&fakeQuotaProbes{})
	code, body := serveQuotaProbes(t, h.GetServerCapabilities, http.MethodGet, "/", "")
	if code != http.StatusOK || body["quota_cache"] != true {
		t.Fatalf("capabilities: %d %v", code, body)
	}
}
