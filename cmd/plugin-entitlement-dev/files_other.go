//go:build !linux && !darwin

package main

import "errors"

func readPrivateRegularFile(string, int64) ([]byte, error) {
	return nil, errors.New("development issuer renewal requires Linux or macOS no-follow file support")
}
