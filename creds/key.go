package creds

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFileName is the filename of the master key within the user config dir.
const KeyFileName = "master.key"

// KeyFilePath returns the absolute path where the master key is stored.
// Location: os.UserConfigDir()/ageage/master.key
// This is intentionally separate from the workspace so that credentials.toml
// cannot be decrypted even if the workspace directory is compromised.
func KeyFilePath() (string, error) {
	dir, err := userKeyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, KeyFileName), nil
}

// userKeyDir returns the platform-specific directory used for the master key.
func userKeyDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine user config dir: %w", err)
	}
	return filepath.Join(base, "ageage"), nil
}

// loadOrGenerateKey returns the 32-byte master key.
// If the key file does not exist it is generated and saved automatically.
func loadOrGenerateKey() ([32]byte, error) {
	dir, err := userKeyDir()
	if err != nil {
		return [32]byte{}, err
	}
	path := filepath.Join(dir, KeyFileName)

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return [32]byte{}, fmt.Errorf("read master key %s: %w", path, err)
		}
		return generateAndSaveKey(dir, path)
	}
	return parseKeyFile(data, path)
}

func generateAndSaveKey(dir, path string) ([32]byte, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return [32]byte{}, fmt.Errorf("generate master key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return [32]byte{}, fmt.Errorf("create key dir %s: %w", dir, err)
	}
	encoded := hex.EncodeToString(key[:]) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return [32]byte{}, fmt.Errorf("save master key to %s: %w", path, err)
	}
	fmt.Printf("[creds] Master key generated and saved to %s\n", path)
	return key, nil
}

// parseKeyFile decodes a hex-encoded 32-byte key from the key file contents.
func parseKeyFile(data []byte, path string) ([32]byte, error) {
	s := strings.TrimRight(string(data), "\r\n ")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, fmt.Errorf(
			"master key file %s is corrupt (expected 64 hex chars, got %d chars)",
			path, len(s),
		)
	}
	var key [32]byte
	copy(key[:], b)
	return key, nil
}
