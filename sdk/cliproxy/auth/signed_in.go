package auth

import "time"

// MetadataKeySignedInAt is the credential metadata field that records the time of the
// latest interactive sign-in, as an RFC 3339 UTC timestamp. Token refreshes keep it;
// only a new sign-in replaces it. Credentials saved before the field existed lack it.
const MetadataKeySignedInAt = "signed_in_at"

// MarkSignedIn records at as the time of the credential's latest interactive sign-in.
// Login orchestrators call it on a fresh login result before it is merged with the
// existing credential and saved, so the new time replaces the old one.
func MarkSignedIn(auth *Auth, at time.Time) {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[MetadataKeySignedInAt] = at.UTC().Format(time.RFC3339)
}

// SignedInAt returns the time of the credential's latest interactive sign-in.
// It reports false when the credential has no valid record of it.
func (a *Auth) SignedInAt() (time.Time, bool) {
	raw, ok := a.Metadata[MetadataKeySignedInAt].(string)
	if !ok {
		return time.Time{}, false
	}
	at, errParse := time.Parse(time.RFC3339, raw)
	if errParse != nil {
		return time.Time{}, false
	}
	return at, true
}
