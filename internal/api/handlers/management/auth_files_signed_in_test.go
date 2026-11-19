package management

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCommitBuiltinOAuthSessionStampsSignedInAt(t *testing.T) {
	authDir := t.TempDir()
	fileName := "kimi-user.json"
	filePath := filepath.Join(authDir, fileName)
	// An earlier sign-in of the same account.
	raw, errMarshal := json.Marshal(map[string]any{
		"type":                         "kimi",
		"access_token":                 "old-token",
		coreauth.MetadataKeySignedInAt: "2026-01-01T00:00:00Z",
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(filePath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)
	state := "signed-in-at-commit"
	RegisterOAuthSession(state, "kimi")
	t.Cleanup(func() { CompleteOAuthSession(state) })

	before := time.Now().Add(-time.Second)
	if _, ok := h.commitAndCompleteBuiltinOAuthSession(context.Background(), state, "kimi", "", &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "kimi",
		Metadata: map[string]any{"type": "kimi", "access_token": "new-token"},
	}, "save failed", false); !ok {
		t.Fatal("commitAndCompleteBuiltinOAuthSession failed")
	}
	after := time.Now()

	saved, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var savedAuth coreauth.Auth
	if errUnmarshal := json.Unmarshal(saved, &savedAuth.Metadata); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	signedInAt, ok := savedAuth.SignedInAt()
	if !ok || signedInAt.Before(before) || signedInAt.After(after) {
		t.Fatalf("saved signed_in_at = %v (%t), want the login time", savedAuth.Metadata[coreauth.MetadataKeySignedInAt], ok)
	}
}

func TestBuildAuthFileEntryExposesSignedInAtOnV8Only(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandler(&config.Config{AuthDir: authDir}, "", nil)
	auth := &coreauth.Auth{
		ID:         "claude-user.json",
		FileName:   "claude-user.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"path": filepath.Join(authDir, "claude-user.json")},
		Metadata:   map[string]any{coreauth.MetadataKeySignedInAt: "2026-10-20T08:15:00Z"},
	}

	if got := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})["signed_in_at"]; got != "2026-10-20T08:15:00Z" {
		t.Fatalf("v8 signed_in_at = %v, want 2026-10-20T08:15:00Z", got)
	}
	if _, exists := h.buildAuthFileEntry(auth, authFileQuotaContext{})["signed_in_at"]; exists {
		t.Fatal("v0 response changed")
	}

	auth.Metadata = map[string]any{"email": "user@example.com"}
	if _, exists := h.buildAuthFileEntry(auth, authFileQuotaContext{v8: true})["signed_in_at"]; exists {
		t.Fatal("signed_in_at reported for a credential without a recorded sign-in")
	}
}
