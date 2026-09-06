package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const secretPrefix = "enc:v1:"

// Codec encrypts and decrypts persisted secrets.
type Codec interface {
	Encrypt(value string) (string, error)
	Decrypt(value string) (string, error)
}

// AESCodec protects secrets with AES-GCM using a locally stored key.
type AESCodec struct {
	aead cipher.AEAD
}

// NewAESCodec derives a codec from an explicit passphrase when provided,
// otherwise from a key file created inside the data directory.
func NewAESCodec(dataDir, passphrase string) (*AESCodec, error) {
	key, err := resolveKey(dataDir, passphrase)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESCodec{aead: aead}, nil
}

func resolveKey(dataDir, passphrase string) ([]byte, error) {
	if passphrase != "" {
		sum := sha256.Sum256([]byte(passphrase))
		return sum[:], nil
	}
	keyPath := filepath.Join(dataDir, "secret.key")
	stored, err := os.ReadFile(keyPath)
	if err == nil {
		key, decodeErr := base64.StdEncoding.DecodeString(string(stored))
		if decodeErr != nil || len(key) != 32 {
			return nil, fmt.Errorf("secret key at %s is invalid", keyPath)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(keyPath, []byte(encoded), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Encrypt returns a prefixed base64 ciphertext for a plaintext secret.
func (c *AESCodec) Encrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(value), nil)
	return secretPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt and passes through values stored before encryption.
func (c *AESCodec) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) <= len(secretPrefix) || value[:len(secretPrefix)] != secretPrefix {
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value[len(secretPrefix):])
	if err != nil {
		return "", err
	}
	if len(raw) < c.aead.NonceSize() {
		return "", errors.New("stored secret is truncated")
	}
	nonce := raw[:c.aead.NonceSize()]
	plaintext, err := c.aead.Open(nil, nonce, raw[c.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("stored secret cannot be decrypted with the current key")
	}
	return string(plaintext), nil
}
