package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// maxManagementKeyInput bounds the stdin read; bcrypt itself rejects keys longer than 72 bytes.
const maxManagementKeyInput = 4096

// DoHashManagementKey reads a management key from in and writes the bcrypt hash for management.secret-key to out.
// One trailing newline is removed, so `echo "$key" | cliproxyapi -hash-management-key` and a file without a newline
// produce the same key. It returns the process exit code.
func DoHashManagementKey(in io.Reader, out, errOut io.Writer) int {
	hash, err := hashManagementKey(in)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "hash-management-key: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(out, hash)
	return 0
}

func hashManagementKey(in io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxManagementKeyInput+1))
	if err != nil {
		return "", fmt.Errorf("read key from stdin: %w", err)
	}
	if len(data) > maxManagementKeyInput {
		return "", fmt.Errorf("key is longer than %d bytes", maxManagementKeyInput)
	}
	key := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if key == "" {
		return "", errors.New("empty key on stdin")
	}
	hash, err := config.HashSecret(key)
	if err != nil {
		return "", fmt.Errorf("hash key: %w", err)
	}
	return hash, nil
}
