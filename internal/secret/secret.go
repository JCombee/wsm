// Package secret encrypts the manager's secrets at rest, such as the
// private parts of deploy keys (plan section 3.12, ADR 0009).
//
// Values are sealed with AES-256-GCM under a 32-byte key from a key file in
// the manager's state directory. The caller passes additional data, such as
// the project ID, that binds a sealed value to its owner: a value sealed for
// one project does not open for another.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// KeySize is the size of the key file in bytes (AES-256).
const KeySize = 32

// KeyFileMode is the only mode a key file may have.
const KeyFileMode = 0o600

// DefaultKeyFile is where "wsm serve" keeps its key.
const DefaultKeyFile = "/var/lib/wsm/secret.key"

// ErrOpen means a sealed value could not be opened: it was changed, sealed
// with another key or for other additional data, or is too short.
var ErrOpen = errors.New("secret: cannot open sealed value")

// Box seals and opens values with one key. It holds no other state.
type Box struct {
	aead cipher.AEAD
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret: key is %d bytes, want %d", len(key), KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and binds it to ad. The result is the random
// nonce followed by the ciphertext and tag.
func (b *Box) Seal(plaintext, ad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize(), b.aead.NonceSize()+len(plaintext)+b.aead.Overhead())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, ad), nil
}

// Open decrypts a value from Seal that was bound to ad.
func (b *Box) Open(sealed, ad []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrOpen
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return out, nil
}

// LoadOrCreateKey returns the key in path. A missing file is created with
// a new random key and mode 0600. An existing file must be a regular file
// of exactly 32 bytes with mode 0600; anything else is an error, and the
// file is left as it is.
func LoadOrCreateKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	switch {
	case !fi.Mode().IsRegular():
		return nil, fmt.Errorf("key file %s is not a regular file", path)
	case fi.Mode().Perm() != KeyFileMode:
		return nil, fmt.Errorf("key file %s has mode %04o, want %04o", path, fi.Mode().Perm(), KeyFileMode)
	case fi.Size() != KeySize:
		return nil, fmt.Errorf("key file %s is %d bytes, want %d", path, fi.Size(), KeySize)
	}
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return key, nil
}

// createKey writes a new random key to path, which must not exist yet.
func createKey(path string) ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("new key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, KeyFileMode)
	if err != nil {
		return nil, fmt.Errorf("create key file: %w", err)
	}
	// The umask can only remove bits, but make the mode exact anyway.
	if err := f.Chmod(KeyFileMode); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("create key file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("create key file: %w", err)
	}
	return key, nil
}
