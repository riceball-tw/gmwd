// Package config locates gmwd's data directory.
package config

import (
	"os"
	"path/filepath"
)

// Home is gmwd's data dir: $GMWD_HOME, or ~/.gmwd.
func Home() (string, error) {
	if h := os.Getenv("GMWD_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gmwd"), nil
}
