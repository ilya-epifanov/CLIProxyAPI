package cmd

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestDoHashManagementKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		key   string
	}{
		{name: "no newline", input: "s3cret", key: "s3cret"},
		{name: "trailing newline", input: "s3cret\n", key: "s3cret"},
		{name: "trailing CRLF", input: "s3cret\r\n", key: "s3cret"},
		{name: "inner and leading spaces kept", input: " s3 cret\n", key: " s3 cret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := DoHashManagementKey(strings.NewReader(tt.input), &out, &errOut); code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, errOut.String())
			}
			hash := strings.TrimSuffix(out.String(), "\n")
			if !strings.HasPrefix(hash, "$2a$") {
				t.Fatalf("output %q is not a bcrypt hash", hash)
			}
			if errCompare := bcrypt.CompareHashAndPassword([]byte(hash), []byte(tt.key)); errCompare != nil {
				t.Fatalf("hash does not match key %q: %v", tt.key, errCompare)
			}
		})
	}
}

func TestDoHashManagementKeyRejectsInvalidInput(t *testing.T) {
	for name, input := range map[string]string{
		"empty":         "",
		"only newline":  "\n",
		"over 72 bytes": strings.Repeat("k", 73),
		"over limit":    strings.Repeat("k", maxManagementKeyInput+1),
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := DoHashManagementKey(strings.NewReader(input), &out, &errOut); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", out.String())
			}
		})
	}
}
