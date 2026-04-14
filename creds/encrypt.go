package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

const nonceSize = 12 // GCM standard nonce length

// encrypt encrypts plaintext with AES-256-GCM.
// Output layout: [12-byte random nonce][ciphertext+GCM tag]
func encrypt(key [32]byte, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	// Seal appends ciphertext+tag to nonce, producing the final blob.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt verifies and decrypts data produced by encrypt.
func decrypt(key [32]byte, data []byte) ([]byte, error) {
	// Minimum: 12-byte nonce + 16-byte GCM tag = 28 bytes (zero-length plaintext).
	if len(data) < nonceSize+16 {
		return nil, fmt.Errorf("credentials data too short (%d bytes); file may be corrupt", len(data))
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed (wrong key or corrupt file): %w", err)
	}
	return plain, nil
}
