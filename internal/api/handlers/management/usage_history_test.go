package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

type fakeUsageHistory struct {
	seriesQuery usagestore.SeriesQuery
	totalsRange usagestore.Range
}

func (f *fakeUsageHistory) Series(_ context.Context, q usagestore.SeriesQuery) ([]usagestore.Series, error) {
	f.seriesQuery = q
	return []usagestore.Series{{
		Key: "claude",
		Points: []usagestore.Point{{
			T:      time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC),
			Counts: usagestore.Counts{Requests: 2, Failed: 1, Tokens: usagestore.Tokens{Input: 10, CacheRead: 500}},
		}},
	}}, nil
}

func (f *fakeUsageHistory) Totals(_ context.Context, r usagestore.Range) ([]usagestore.TotalsRow, error) {
	f.totalsRange = r
	return []usagestore.TotalsRow{{Provider: "claude", AuthIndex: "a1", Model: "claude-fable-5", Counts: usagestore.Counts{Requests: 3}}}, nil
}

func (f *fakeUsageHistory) DroppedRecords() uint64 { return 7 }

func serveUsage(t *testing.T, h *Handler, handler func(*gin.Context), target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	handler(c)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestUsageHistoryEndpointsWithoutStore(t *testing.T) {
	h := &Handler{}
	for name, handler := range map[string]func(*gin.Context){"series": h.GetUsageSeries, "totals": h.GetUsageTotals} {
		code, body := serveUsage(t, h, handler, "/?from=2026-10-09T00:00:00Z&to=2026-10-10T00:00:00Z&bucket=1h&group_by=provider")
		if code != http.StatusNotFound || body["error"] != "usage_store_disabled" {
			t.Fatalf("%s: status %d body %v, want 404 usage_store_disabled", name, code, body)
		}
	}
	if got := h.usageStoreCapability(); got["enabled"] != false || len(got) != 1 {
		t.Fatalf("capability = %v, want only enabled=false", got)
	}
}

func TestGetUsageSeries(t *testing.T) {
	history := &fakeUsageHistory{}
	h := &Handler{}
	h.SetUsageHistory(history)

	code, body := serveUsage(t, h, h.GetUsageSeries, "/?from=2026-10-09T00:30:00Z&to=2026-10-09T12:10:00Z&bucket=1d&tz=Europe/Amsterdam&group_by=model")
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	if body["bucket"] != "1d" || body["group_by"] != "model" || body["from"] != "2026-10-09T00:00:00Z" || body["to"] != "2026-10-09T13:00:00Z" {
		t.Fatalf("envelope = %v", body)
	}
	point := body["series"].([]any)[0].(map[string]any)["points"].([]any)[0].(map[string]any)
	if point["t"] != "2026-10-09T10:00:00Z" || point["requests"] != float64(2) || point["cache_read"] != float64(500) || point["output"] != float64(0) {
		t.Fatalf("point = %v", point)
	}
	if history.seriesQuery.GroupBy != usagestore.DimensionModel || history.seriesQuery.Location.String() != "Europe/Amsterdam" {
		t.Fatalf("query = %+v", history.seriesQuery)
	}
}

func TestGetUsageSeriesRejectsInvalidQuery(t *testing.T) {
	h := &Handler{}
	h.SetUsageHistory(&fakeUsageHistory{})
	code, body := serveUsage(t, h, h.GetUsageSeries, "/?from=2026-10-09T00:00:00Z&to=2026-10-10T00:00:00Z&bucket=1h&group_by=api_key")
	if code != http.StatusBadRequest || body["error"] != "invalid_query" || body["message"] == "" {
		t.Fatalf("status %d body %v, want 400 invalid_query with a message", code, body)
	}
}

func TestGetUsageTotals(t *testing.T) {
	history := &fakeUsageHistory{}
	h := &Handler{}
	h.SetUsageHistory(history)

	code, body := serveUsage(t, h, h.GetUsageTotals, "/?from=2026-10-08T10:00:00Z&to=2026-10-09T10:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	row := body["rows"].([]any)[0].(map[string]any)
	if row["provider"] != "claude" || row["auth_index"] != "a1" || row["model"] != "claude-fable-5" || row["requests"] != float64(3) {
		t.Fatalf("row = %v", row)
	}
	if got := h.usageStoreCapability(); got["enabled"] != true || got["dropped_records"] != uint64(7) {
		t.Fatalf("capability = %v", got)
	}
}
