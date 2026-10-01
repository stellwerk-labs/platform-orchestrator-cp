//go:build linux || darwin

package pluginentitlements

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// readRegularFile opens every path component relative to its already opened
// parent, forbidding symlinks throughout the path. The size check is repeated
// during the read so a growing file cannot evade the bound.
func readRegularFile(path string, maxBytes int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(os.PathSeparator) || maxBytes <= 0 {
		return nil, errors.New("entitlement file path must be a clean absolute file path")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator))
	dir, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open entitlement path root: %w", err)
	}
	defer func() { _ = unix.Close(dir) }()
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid entitlement path component")
		}
		next, err := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("open entitlement directory: %w", err)
		}
		_ = unix.Close(dir)
		dir = next
	}
	name := parts[len(parts)-1]
	if name == "" || name == "." || name == ".." {
		return nil, errors.New("invalid entitlement filename")
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open entitlement file: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return nil, errors.New("entitlement file must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || int64(len(raw)) > maxBytes {
		return nil, errors.New("entitlement file exceeds bound or is empty")
	}
	return raw, nil
}
