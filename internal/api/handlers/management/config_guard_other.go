//go:build !unix

package management

import "os"

// configFileWritable opens the file for writing without truncating it, the same way WriteConfig does.
func configFileWritable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
