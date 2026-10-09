package usage

import (
	"context"
	"net/http"
	"strings"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// UnknownValue labels a record whose provider or model was not reported.
const UnknownValue = "unknown"

// Resolved is a Record whose context-dependent fields are settled, so every
// usage sink classifies, attributes and counts a record the same way.
//
// Compared to the published Record:
//   - Provider and Model are trimmed, valid UTF-8 and never empty (UnknownValue
//     instead). Model can come from client input (an image tool's model).
//   - RequestedAt is never zero.
//   - Stream also reflects the request context.
//   - SessionID and ParentSessionID come from the record or the client request
//     metadata, normalized to canonical UUIDs; ParentSessionID is empty when it
//     equals SessionID or SessionID is empty.
//   - Failed also reflects an error response status in the context, and
//     Fail.StatusCode is http.StatusOK on success and never zero on failure.
//   - Detail carries a valid canonical TokenBreakdown.
type Resolved struct {
	Record
}

// Resolve settles the fields of record that sinks would otherwise derive from ctx.
func Resolve(ctx context.Context, record Record) Resolved {
	record.Detail = EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	record.Provider = valueOrUnknown(record.Provider)
	record.Model = valueOrUnknown(record.Model)
	if record.RequestedAt.IsZero() {
		record.RequestedAt = time.Now()
	}
	if !record.Stream {
		record.Stream = StreamFromContext(ctx)
	}
	record.SessionID, record.ParentSessionID = resolveSessionIDs(ctx, record)
	record.Failed, record.Fail = resolveOutcome(ctx, record)
	return Resolved{Record: record}
}

func valueOrUnknown(value string) string {
	if trimmed := strings.TrimSpace(strings.ToValidUTF8(value, "\uFFFD")); trimmed != "" {
		return trimmed
	}
	return UnknownValue
}

func resolveSessionIDs(ctx context.Context, record Record) (string, string) {
	clientRequestMetadata := internallogging.GetClientRequestMetadata(ctx)
	sessionID := strings.TrimSpace(record.SessionID)
	parentSessionID := strings.TrimSpace(record.ParentSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(clientRequestMetadata.SessionID)
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	} else if parentSessionID == "" && sessionID == strings.TrimSpace(clientRequestMetadata.SessionID) {
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)
	parentSessionID = coresession.NormalizeToCanonicalUUID(parentSessionID)
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}
	return sessionID, parentSessionID
}

// resolveOutcome treats a record as failed when the executor marked it so or
// when the inbound response already carries an error status.
func resolveOutcome(ctx context.Context, record Record) (bool, Failure) {
	responseStatus := internallogging.GetResponseStatus(ctx)
	failed := record.Failed || responseStatus >= http.StatusBadRequest
	if !failed {
		return false, Failure{StatusCode: http.StatusOK}
	}
	fail := Failure{
		StatusCode: record.Fail.StatusCode,
		Body:       strings.TrimSpace(record.Fail.Body),
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = responseStatus
	}
	if fail.StatusCode <= 0 {
		fail.StatusCode = http.StatusInternalServerError
	}
	return true, fail
}
