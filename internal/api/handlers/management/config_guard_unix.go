//go:build unix

package management

import "golang.org/x/sys/unix"

// configFileWritable uses access(2), so the check opens nothing and the config watcher sees no event.
func configFileWritable(path string) bool {
	return unix.Access(path, unix.W_OK) == nil
}
