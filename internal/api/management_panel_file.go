package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// serveManagementPanelFile serves the control panel with an ETag of its
// content and Cache-Control: no-cache, so a browser revalidates on every load
// and gets a replaced panel at once. The file's modification time is not sent:
// package managers and image builds often give every version the same one (Nix
// uses 1970-01-01), and a browser then keeps an old panel after a 304.
func serveManagementPanelFile(c *gin.Context, filePath string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		log.WithError(err).Error("failed to read management control panel asset")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(data)
	c.Header("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	c.Header("Cache-Control", "no-cache")
	// A zero time omits Last-Modified; ServeContent answers If-None-Match with 304.
	http.ServeContent(c.Writer, c.Request, filepath.Base(filePath), time.Time{}, bytes.NewReader(data))
}
