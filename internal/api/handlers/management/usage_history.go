package management

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	log "github.com/sirupsen/logrus"
)

// UsageHistory answers usage history queries; *usagestore.Store implements it.
type UsageHistory interface {
	Series(ctx context.Context, q usagestore.SeriesQuery) ([]usagestore.Series, error)
	Totals(ctx context.Context, r usagestore.Range) ([]usagestore.TotalsRow, error)
	DroppedRecords() uint64
}

// SetUsageHistory sets the usage store behind the usage history endpoints.
// Without one, they answer 404 usage_store_disabled.
func (h *Handler) SetUsageHistory(history UsageHistory) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.usageHistory = history
	h.mu.Unlock()
}

func (h *Handler) currentUsageHistory() UsageHistory {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usageHistory
}

// usageStoreCapability is the usage_store entry of GET /server/capabilities.
func (h *Handler) usageStoreCapability() gin.H {
	history := h.currentUsageHistory()
	if history == nil {
		return gin.H{"enabled": false}
	}
	return gin.H{"enabled": true, "dropped_records": history.DroppedRecords()}
}

// GetUsageSeries returns usage over time grouped by provider, auth_index or model.
func (h *Handler) GetUsageSeries(c *gin.Context) {
	history := h.currentUsageHistory()
	if history == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "usage_store_disabled"})
		return
	}
	q, err := usagestore.ParseSeriesQuery(c.Query("from"), c.Query("to"), c.Query("bucket"), c.Query("tz"), c.Query("group_by"))
	if err != nil {
		writeInvalidUsageQuery(c, err)
		return
	}
	series, err := history.Series(c.Request.Context(), q)
	if err != nil {
		writeUsageQueryFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"bucket":   q.Bucket.String(),
		"group_by": q.GroupBy.String(),
		"from":     q.Range.From,
		"to":       q.Range.To,
		"series":   series,
	})
}

// GetUsageTotals returns usage per provider, auth_index and model over a range.
func (h *Handler) GetUsageTotals(c *gin.Context) {
	history := h.currentUsageHistory()
	if history == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "usage_store_disabled"})
		return
	}
	r, err := usagestore.ParseRange(c.Query("from"), c.Query("to"))
	if err != nil {
		writeInvalidUsageQuery(c, err)
		return
	}
	rows, err := history.Totals(c.Request.Context(), r)
	if err != nil {
		writeUsageQueryFailed(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"from": r.From, "to": r.To, "rows": rows})
}

func writeInvalidUsageQuery(c *gin.Context, err error) {
	var queryErr *usagestore.QueryError
	if !errors.As(err, &queryErr) {
		writeUsageQueryFailed(c, err)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_query", "message": queryErr.Error()})
}

func writeUsageQueryFailed(c *gin.Context, err error) {
	log.WithError(err).Error("management: usage history query failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "usage_query_failed"})
}
