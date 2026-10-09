package usage

import (
	"context"
	"net/http"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func contextWithResponseStatus(status int) context.Context {
	ctx := internallogging.WithResponseStatusHolder(context.Background())
	internallogging.SetResponseStatus(ctx, status)
	return ctx
}

func TestResolveOutcome(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		record     Record
		wantFailed bool
		wantStatus int
	}{
		{name: "success without status", ctx: context.Background(), wantStatus: http.StatusOK},
		{name: "success with ok status", ctx: contextWithResponseStatus(http.StatusOK), wantStatus: http.StatusOK},
		{name: "context error status", ctx: contextWithResponseStatus(http.StatusTooManyRequests), wantFailed: true, wantStatus: http.StatusTooManyRequests},
		{name: "record status wins", ctx: contextWithResponseStatus(http.StatusBadGateway), record: Record{Failed: true, Fail: Failure{StatusCode: http.StatusUnauthorized}}, wantFailed: true, wantStatus: http.StatusUnauthorized},
		{name: "failure without any status", ctx: context.Background(), record: Record{Failed: true}, wantFailed: true, wantStatus: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.ctx, tt.record)
			if got.Failed != tt.wantFailed || got.Fail.StatusCode != tt.wantStatus {
				t.Fatalf("Resolve() failed=%v status=%d, want failed=%v status=%d", got.Failed, got.Fail.StatusCode, tt.wantFailed, tt.wantStatus)
			}
		})
	}
}

func TestResolveFillsDefaults(t *testing.T) {
	got := Resolve(WithStream(context.Background(), true), Record{Provider: "  ", Model: " gpt-5 "})
	if got.Provider != UnknownValue || got.Model != "gpt-5" {
		t.Fatalf("Resolve() provider=%q model=%q, want %q and %q", got.Provider, got.Model, UnknownValue, "gpt-5")
	}
	if got.RequestedAt.IsZero() {
		t.Fatal("Resolve() left RequestedAt zero")
	}
	if !got.Stream {
		t.Fatal("Resolve() ignored the stream flag in the context")
	}
	if !got.Detail.TokenBreakdown.Valid() {
		t.Fatalf("Resolve() breakdown is not valid: %+v", got.Detail.TokenBreakdown)
	}
}

func TestResolveReplacesInvalidUTF8(t *testing.T) {
	if got := Resolve(context.Background(), Record{Model: "img-\xff"}); got.Model != "img-\uFFFD" {
		t.Fatalf("Resolve() Model = %q, want invalid bytes replaced", got.Model)
	}
}

func TestResolveKeepsRequestedAt(t *testing.T) {
	at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	if got := Resolve(context.Background(), Record{RequestedAt: at}); !got.RequestedAt.Equal(at) {
		t.Fatalf("Resolve() RequestedAt = %v, want %v", got.RequestedAt, at)
	}
}

func TestResolveSessionIDs(t *testing.T) {
	const session = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		SessionID:       session,
		ParentSessionID: session,
	})
	got := Resolve(ctx, Record{})
	if got.SessionID != session {
		t.Fatalf("Resolve() SessionID = %q, want %q", got.SessionID, session)
	}
	if got.ParentSessionID != "" {
		t.Fatalf("Resolve() ParentSessionID = %q, want empty when equal to the session", got.ParentSessionID)
	}
}
