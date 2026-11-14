package gitmgr

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
)

// DeployKey is a new ed25519 deploy key. Private is the key in OpenSSH's
// own format, as ssh-keygen writes it. It is secret: it is stored only
// sealed (ADR 0009) and written to disk only for the duration of one git
// command.
type DeployKey struct {
	// Public is the authorized_keys line: "ssh-ed25519 <base64> <comment>".
	Public  string
	Private []byte
}

// sshString appends an SSH wire-format string: a uint32 length and the bytes.
func sshString(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

const keyType = "ssh-ed25519"

// NewDeployKey creates an ed25519 key pair with the standard library only.
// comment ends the public key line and must not contain a newline.
func NewDeployKey(comment string) (DeployKey, error) {
	for _, r := range comment {
		if r < 0x20 || r == 0x7f {
			return DeployKey{}, fmt.Errorf("deploy key comment contains control characters")
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return DeployKey{}, fmt.Errorf("generate deploy key: %w", err)
	}
	pubBlob := sshString(sshString(nil, []byte(keyType)), pub)

	// The private section (PROTOCOL.key in OpenSSH): two equal check
	// integers, the key, the comment, and padding 1, 2, 3... up to a
	// multiple of the block size, 8 for the cipher "none".
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return DeployKey{}, fmt.Errorf("generate deploy key: %w", err)
	}
	sec := append(check[:], check[:]...)
	sec = sshString(sec, []byte(keyType))
	sec = sshString(sec, pub)
	sec = sshString(sec, priv) // seed followed by the public key, 64 bytes
	sec = sshString(sec, []byte(comment))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}

	blob := []byte("openssh-key-v1\x00")
	blob = sshString(blob, []byte("none")) // cipher
	blob = sshString(blob, []byte("none")) // KDF
	blob = sshString(blob, nil)            // KDF options
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = sshString(blob, pubBlob)
	blob = sshString(blob, sec)

	return DeployKey{
		Public:  keyType + " " + base64.StdEncoding.EncodeToString(pubBlob) + " " + comment,
		Private: pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: blob}),
	}, nil
}
