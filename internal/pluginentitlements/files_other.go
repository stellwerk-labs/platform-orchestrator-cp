//go:build !linux && !darwin

package pluginentitlements

import "errors"

func readRegularFile(string, int64) ([]byte, error) {
	return nil, errors.New("entitlement file loading requires no-symlink open support")
}
