package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestManagerLoginStampsSignedInAt(t *testing.T) {
	for name, existing := range map[string]map[string]any{
		"new credential": nil,
		"re-login":       {"type": "demo", "access_token": "old-token", coreauth.MetadataKeySignedInAt: "2026-01-01T00:00:00Z"},
	} {
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			filePath := filepath.Join(authDir, "demo.json")
			if existing != nil {
				writeCodexRefreshFixture(t, filePath, existing)
			}
			store := NewFileTokenStore()
			mgr := NewManager(store, &dummyAuthenticator{provider: "demo", record: &coreauth.Auth{
				ID:       "demo.json",
				FileName: "demo.json",
				Provider: "demo",
				Metadata: map[string]any{"type": "demo", "access_token": "new-token"},
			}})

			before := time.Now().Add(-time.Second)
			if _, _, errLogin := mgr.Login(context.Background(), "demo", &config.Config{AuthDir: authDir}, nil); errLogin != nil {
				t.Fatalf("Login error: %v", errLogin)
			}
			after := time.Now()

			saved := coreauth.Auth{Metadata: readCodexRefreshFixture(t, filePath)}
			signedInAt, ok := saved.SignedInAt()
			if !ok || signedInAt.Before(before) || signedInAt.After(after) {
				t.Fatalf("saved signed_in_at = %v (%t), want the login time", saved.Metadata[coreauth.MetadataKeySignedInAt], ok)
			}
		})
	}
}

func TestFileTokenStoreCodexRefreshPreservesSignedInAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	base := codexRefreshFixture()
	base.Metadata[coreauth.MetadataKeySignedInAt] = "2026-10-01T00:00:00Z"
	writeCodexRefreshFixture(t, path, base.Metadata)

	updated := base.Clone()
	updated.Metadata["access_token"] = "outer-new"
	updated.Metadata["refresh_token"] = "outer-ref-new"
	updated.CodexRefreshPatch = codex.NewCredentialRefreshPatch(base.Metadata, updated.Metadata, true, false)
	updated.Attributes = map[string]string{"path": path}

	if _, errSave := NewFileTokenStore().Save(context.Background(), updated); errSave != nil {
		t.Fatalf("Save error: %v", errSave)
	}

	saved := readCodexRefreshFixture(t, path)
	if saved["access_token"] != "outer-new" {
		t.Fatalf("access_token = %v, want outer-new", saved["access_token"])
	}
	if got := saved[coreauth.MetadataKeySignedInAt]; got != "2026-10-01T00:00:00Z" {
		t.Fatalf("signed_in_at = %v, want the original sign-in time", got)
	}
}
