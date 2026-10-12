package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaprobe"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// maxQuotaProbeBody bounds one stored upstream answer.
const maxQuotaProbeBody = 1 << 20

// QuotaProbes serves server-side quota reads; *quotaprobe.Prober implements it.
type QuotaProbes interface {
	Entries() []quotaprobe.Entry
	Refresh(ctx context.Context, authIndex string) (quotaprobe.Entry, error)
}

// SetQuotaProbes sets the prober behind the quota probe endpoints. Without one,
// they answer 404 quota_probes_disabled.
func (h *Handler) SetQuotaProbes(probes QuotaProbes) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.quotaProbes = probes
	h.mu.Unlock()
}

func (h *Handler) currentQuotaProbes() QuotaProbes {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quotaProbes
}

// DoQuotaProbe implements quotaprobe.Doer with the credentials, proxy and
// timeout that APICall uses.
func (h *Handler) DoQuotaProbe(ctx context.Context, auth *coreauth.Auth, request quotaprobe.Request) (quotaprobe.Response, error) {
	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if token == "" {
		if errToken != nil {
			return quotaprobe.Response{}, fmt.Errorf("auth token refresh failed: %w", errToken)
		}
		return quotaprobe.Response{}, errors.New("auth token not found")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return quotaprobe.Response{}, fmt.Errorf("build %s request: %w", request.Name, err)
	}
	for key, value := range request.Header {
		req.Header.Set(key, value)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth, "")}
	resp, err := client.Do(req)
	if err != nil {
		return quotaprobe.Response{}, fmt.Errorf("%s request failed: %w", request.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaProbeBody))
	if err != nil {
		return quotaprobe.Response{}, fmt.Errorf("read %s response: %w", request.Name, err)
	}
	return quotaprobe.Response{StatusCode: resp.StatusCode, Body: string(body)}, nil
}

// GetQuotaProbes returns the latest quota read of every account. It never
// contacts a provider.
func (h *Handler) GetQuotaProbes(c *gin.Context) {
	probes := h.currentQuotaProbes()
	if probes == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota_probes_disabled"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": probes.Entries()})
}

type refreshQuotaProbeRequest struct {
	AuthIndex string `json:"auth_index"`
}

// RefreshQuotaProbe reads one account's quota now, unless it was read within
// the minimum interval, and returns the account's entry either way.
func (h *Handler) RefreshQuotaProbe(c *gin.Context) {
	probes := h.currentQuotaProbes()
	if probes == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "quota_probes_disabled"})
		return
	}
	var body refreshQuotaProbeRequest
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.AuthIndex) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "message": "auth_index is required"})
		return
	}
	entry, err := probes.Refresh(c.Request.Context(), body.AuthIndex)
	switch {
	case errors.Is(err, quotaprobe.ErrUnknownAuth):
		c.JSON(http.StatusNotFound, gin.H{"error": "credential_not_found"})
	case errors.Is(err, quotaprobe.ErrUnsupportedProvider):
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider_not_supported"})
	case err != nil:
		// The client went away while the probe was queued.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "request_canceled"})
	default:
		c.JSON(http.StatusOK, entry)
	}
}
