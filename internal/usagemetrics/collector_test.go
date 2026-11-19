package usagemetrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type fakeStoreStats struct{ dropped, writeErrors uint64 }

func (f fakeStoreStats) DroppedRecords() uint64 { return f.dropped }
func (f fakeStoreStats) WriteErrors() uint64    { return f.writeErrors }

func TestCollectorReportsRequestsAndTokens(t *testing.T) {
	aggregator := NewAggregator()
	record := coreusage.Record{
		Provider:  "claude",
		AuthIndex: "a1",
		Model:     "claude-fable-5",
		// Claude reports cache tokens separately from input.
		Detail: coreusage.Detail{InputTokens: 10, CacheReadTokens: 500, CacheCreationTokens: 40, OutputTokens: 70},
	}
	aggregator.HandleUsage(context.Background(), record)
	aggregator.HandleUsage(context.Background(), record)
	failed := coreusage.Record{Provider: "claude", AuthIndex: "a1", Model: "claude-fable-5", Failed: true, Fail: coreusage.Failure{StatusCode: http.StatusTooManyRequests}}
	aggregator.HandleUsage(context.Background(), failed)

	c := &collector{aggregator: aggregator}
	want := `
# HELP cliproxyapi_upstream_requests_total Upstream request attempts by provider, credential, model and HTTP status (200 on success).
# TYPE cliproxyapi_upstream_requests_total counter
cliproxyapi_upstream_requests_total{auth_index="a1",code="200",model="claude-fable-5",provider="claude"} 2
cliproxyapi_upstream_requests_total{auth_index="a1",code="429",model="claude-fable-5",provider="claude"} 1
# HELP cliproxyapi_upstream_tokens_total Upstream tokens by provider, credential, model and type. Types do not overlap: input excludes cached input and output excludes reasoning.
# TYPE cliproxyapi_upstream_tokens_total counter
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="cache_read"} 1000
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="cache_write"} 80
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="input"} 20
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="output"} 140
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="reasoning"} 0
cliproxyapi_upstream_tokens_total{auth_index="a1",model="claude-fable-5",provider="claude",type="unclassified"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorSurvivesInvalidUTF8(t *testing.T) {
	aggregator := NewAggregator()
	aggregator.HandleUsage(context.Background(), coreusage.Record{Provider: "codex", AuthIndex: "a1", Model: "img-\xff"})
	auths := func() []*coreauth.Auth {
		return []*coreauth.Auth{{Index: "a1", Provider: "codex", Label: "bad-\xfe"}}
	}
	c := &collector{aggregator: aggregator, auths: auths}
	if n := testutil.CollectAndCount(c); n != 8 {
		t.Fatalf("collected %d metrics, want 1 request, 6 token and 1 info series", n)
	}
}

func TestCollectorReportsAuthInfoWithoutAPIKeys(t *testing.T) {
	auths := func() []*coreauth.Auth {
		return []*coreauth.Auth{
			{Index: "a1", Provider: "claude", Label: "work", Metadata: map[string]any{"email": "me@example.com"}},
			{Index: "a1", Provider: "claude", Label: "duplicate"},
			{Index: "k1", Provider: "openai-compatibility", Label: "local", Attributes: map[string]string{"api_key": "sk-secret"}},
		}
	}
	c := &collector{aggregator: NewAggregator(), auths: auths, store: fakeStoreStats{dropped: 3, writeErrors: 1}}
	want := `
# HELP cliproxyapi_auth_info Names for an auth_index; always 1.
# TYPE cliproxyapi_auth_info gauge
cliproxyapi_auth_info{auth_index="a1",email="me@example.com",label="work",provider="claude"} 1
cliproxyapi_auth_info{auth_index="k1",email="",label="local",provider="openai-compatibility"} 1
# HELP cliproxyapi_usage_store_dropped_records_total Usage records the store discarded because its writer fell behind or it was closed.
# TYPE cliproxyapi_usage_store_dropped_records_total counter
cliproxyapi_usage_store_dropped_records_total 3
# HELP cliproxyapi_usage_store_write_errors_total Usage records lost because a store write failed.
# TYPE cliproxyapi_usage_store_write_errors_total counter
cliproxyapi_usage_store_write_errors_total 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerServesRuntimeMetricsWithoutAStore(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(NewAggregator(), nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "go_goroutines ") {
		t.Fatal("response has no Go runtime metrics")
	}
	if strings.Contains(body, "cliproxyapi_usage_store_") {
		t.Fatal("response has store metrics although no store is configured")
	}
}
