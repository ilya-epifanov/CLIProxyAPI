package auth

import (
	"testing"
	"time"
)

func TestMarkSignedInRecordsUTCTimestamp(t *testing.T) {
	auth := &Auth{}
	at := time.Date(2026, 10, 20, 10, 15, 0, 0, time.FixedZone("CEST", 2*60*60))

	MarkSignedIn(auth, at)

	if got := auth.Metadata[MetadataKeySignedInAt]; got != "2026-10-20T08:15:00Z" {
		t.Fatalf("signed_in_at = %v, want 2026-10-20T08:15:00Z", got)
	}
	got, ok := auth.SignedInAt()
	if !ok || !got.Equal(at) {
		t.Fatalf("SignedInAt() = %v, %t, want %v, true", got, ok, at)
	}
}

func TestSignedInAtAbsentOrInvalid(t *testing.T) {
	for name, metadata := range map[string]map[string]any{
		"no metadata": nil,
		"missing":     {"email": "user@example.com"},
		"not a time":  {MetadataKeySignedInAt: "yesterday"},
		"not string":  {MetadataKeySignedInAt: float64(1760000000)},
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := (&Auth{Metadata: metadata}).SignedInAt(); ok {
				t.Fatalf("SignedInAt() = %v, true, want false", got)
			}
		})
	}
}

func TestMergeExistingAuthMetadataKeepsNewSignInTime(t *testing.T) {
	relogin := &Auth{Metadata: map[string]any{"access_token": "new"}}
	MarkSignedIn(relogin, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC))

	MergeExistingAuthMetadata(relogin, map[string]any{
		"access_token":        "old",
		"note":                "keep",
		MetadataKeySignedInAt: "2026-10-01T00:00:00Z",
	})

	if got := relogin.Metadata[MetadataKeySignedInAt]; got != "2026-11-19T00:00:00Z" {
		t.Fatalf("signed_in_at = %v, want the re-login time", got)
	}
	if got := relogin.Metadata["note"]; got != "keep" {
		t.Fatalf("note = %v, want keep", got)
	}
}

func TestMergeRefreshedAuthPreservesSignedInAt(t *testing.T) {
	base := &Auth{ID: "a", Metadata: map[string]any{
		"access_token":        "old",
		MetadataKeySignedInAt: "2026-10-01T00:00:00Z",
	}}
	current := base.Clone()
	// Executors renew tokens in place on a copy of the credential.
	updated := base.Clone()
	updated.Metadata["access_token"] = "new"
	updated.Metadata["last_refresh"] = "2026-11-19T00:00:00Z"

	merged := MergeRefreshedAuth(base, current, updated)

	if got := merged.Metadata["access_token"]; got != "new" {
		t.Fatalf("access_token = %v, want new", got)
	}
	if got := merged.Metadata[MetadataKeySignedInAt]; got != "2026-10-01T00:00:00Z" {
		t.Fatalf("signed_in_at = %v, want the original sign-in time", got)
	}
}
