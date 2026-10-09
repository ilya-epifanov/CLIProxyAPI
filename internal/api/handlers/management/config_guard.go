package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// errConfigReadOnly is the error code returned when a request would write a config file that the server cannot write.
const errConfigReadOnly = "config_read_only"

// nonConfigMutations lists the mutating /v8/management routes that never write the config file.
// Every other PUT, PATCH, DELETE or POST route behind ConfigWriteGuard counts as a config write,
// so a new route is refused on a read-only config until it is listed here.
// The deprecated /v0/management routes are not guarded.
var nonConfigMutations = map[string]struct{}{
	"POST /v8/management/requests/api-call":      {},
	"POST /v8/management/routing/cooldown/reset": {},
	"DELETE /v8/management/observability/logs":   {},
	"POST /v8/management/credentials":            {},
	"DELETE /v8/management/credentials":          {},
	// The config API key branch of the status toggle writes the config; PatchAuthFileStatus guards it itself.
	"PATCH /v8/management/credentials/status": {},
	"PATCH /v8/management/credentials/fields": {},
	"POST /v8/management/credentials/refresh": {},
	"POST /v8/management/oauth/import":        {},
	"DELETE /v8/management/oauth/session":     {},
	"POST /v8/management/plugins/:id/quota":   {},
	"DELETE /v8/management/plugins/:id/quota": {},
}

// NonConfigMutationRoutes returns the routes exempt from ConfigWriteGuard as "METHOD /full/path" strings.
func NonConfigMutationRoutes() []string {
	routes := make([]string, 0, len(nonConfigMutations))
	for route := range nonConfigMutations {
		routes = append(routes, route)
	}
	return routes
}

// ConfigWritable reports whether the server can write its config file.
// It is checked on every call, so a remount or permission change takes effect without a restart.
func (h *Handler) ConfigWritable() bool {
	if h == nil || h.configFilePath == "" {
		return false
	}
	return configFileWritable(h.configFilePath)
}

// rejectReadOnlyConfig responds 409 config_read_only and returns true when the config file is not writable.
// Handlers call it before they change h.cfg, because a failed save does not roll the in-memory config back.
func (h *Handler) rejectReadOnlyConfig(c *gin.Context) bool {
	if h.ConfigWritable() {
		return false
	}
	c.AbortWithStatusJSON(http.StatusConflict, gin.H{
		"error":   errConfigReadOnly,
		"message": "the config file is read-only; change it where it is managed and restart the server",
	})
	return true
}

// ConfigWriteGuard refuses config-writing management requests with 409 config_read_only when the config
// file is not writable. It must run after Middleware, so unauthenticated callers learn nothing about the config.
func (h *Handler) ConfigWriteGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if _, exempt := nonConfigMutations[c.Request.Method+" "+c.FullPath()]; exempt {
			c.Next()
			return
		}
		if h.rejectReadOnlyConfig(c) {
			return
		}
		c.Next()
	}
}

// GetServerCapabilities reports server features that the management panel cannot infer from other endpoints.
func (h *Handler) GetServerCapabilities(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"config_writable": h.ConfigWritable(),
		"strategies":      routingStrategies,
		"usage_store":     h.usageStoreCapability(),
	})
}
