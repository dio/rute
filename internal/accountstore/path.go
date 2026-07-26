package accountstore

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultStateDir returns rute's platform-native state directory.
func DefaultStateDir() (string, error) {
	if runtime.GOOS == "linux" {
		if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
			if !filepath.IsAbs(stateHome) {
				return "", errors.New("accountstore: XDG_STATE_HOME must be absolute")
			}
			return filepath.Join(stateHome, "rute"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", "rute"), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "rute"), nil
}

// DefaultCredentialsPath returns the default plaintext credential file.
func DefaultCredentialsPath() (string, error) {
	dir, err := DefaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// DefaultManifestPath returns the keyring account metadata path.
func DefaultManifestPath() (string, error) {
	dir, err := DefaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts.json"), nil
}
