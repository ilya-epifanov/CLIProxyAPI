package management

import (
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// throttleOverrideJSON is a pool's throttle override as the ranking reports it.
type throttleOverrideJSON struct {
	Speed float64   `json:"speed"`
	SetAt time.Time `json:"set_at"`
}

func throttleOverrideFor(o coreauth.ThrottleOverride) *throttleOverrideJSON {
	return &throttleOverrideJSON{Speed: o.Speed, SetAt: o.SetAt.UTC()}
}

// throttleOverridePool returns the provider pool named in the path, or writes 404.
func (h *Handler) throttleOverridePool(c *gin.Context) (string, bool) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth_manager_unavailable"})
		return "", false
	}
	provider := c.Param("provider")
	if !slices.Contains(rankedProviders, provider) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown_pool"})
		return "", false
	}
	return provider, true
}

// PutThrottleOverride replaces a pool's computed throttle speed until it is cleared:
// PUT /routing/throttle/overrides/:provider {"speed": 0..1}.
func (h *Handler) PutThrottleOverride(c *gin.Context) {
	provider, ok := h.throttleOverridePool(c)
	if !ok {
		return
	}
	var req struct {
		Speed *float64 `json:"speed"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Speed == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "speed is required"})
		return
	}
	if err := coreauth.ValidateThrottleSpeed(*req.Speed); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	override, err := h.authManager.ThrottleOverrides().Set(provider, *req.Speed, time.Now())
	if err != nil {
		log.WithError(err).Error("pace throttle: store override")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store_failed"})
		return
	}
	log.Infof("pace throttle: %s pool overridden to speed %.2f", provider, override.Speed)
	c.JSON(http.StatusOK, gin.H{"provider": provider, "override": throttleOverrideFor(override)})
}

// DeleteThrottleOverride returns a pool to its computed throttle:
// DELETE /routing/throttle/overrides/:provider.
func (h *Handler) DeleteThrottleOverride(c *gin.Context) {
	provider, ok := h.throttleOverridePool(c)
	if !ok {
		return
	}
	if err := h.authManager.ThrottleOverrides().Clear(provider); err != nil {
		log.WithError(err).Error("pace throttle: store override")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "store_failed"})
		return
	}
	log.Infof("pace throttle: %s pool override cleared", provider)
	c.JSON(http.StatusOK, gin.H{"provider": provider, "override": nil})
}
