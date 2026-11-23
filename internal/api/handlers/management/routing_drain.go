package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// PutRoutingDrain flags an account for draining, or clears its flag:
// PUT /routing/drain/:auth_index {"drain": true|false}.
func (h *Handler) PutRoutingDrain(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth_manager_unavailable"})
		return
	}
	authIndex := c.Param("auth_index")
	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	var req struct {
		Drain *bool `json:"drain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Drain == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "drain is required"})
		return
	}
	if err := h.authManager.DrainFlags().Set(auth.ID, *req.Drain, time.Now()); err != nil {
		log.WithError(err).Error("drain: store flags")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store_failed"})
		return
	}
	log.WithField("auth_index", auth.Index).Infof("drain: flag set to %t", *req.Drain)
	c.JSON(http.StatusOK, gin.H{"auth_index": auth.Index, "drain": *req.Drain})
}
