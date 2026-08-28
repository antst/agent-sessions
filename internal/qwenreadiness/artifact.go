package qwenreadiness

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
)

const maximumArtifactBytes = 16 * 1024 * 1024

// ArtifactAttestation binds one bounded Qwen protocol artifact to its content
// and durable filesystem identity.
type ArtifactAttestation struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
}

// AttestArtifact captures the bounded content and durable identity of one
// regular non-symlink Qwen protocol file.
func AttestArtifact(path string) (ArtifactAttestation, error) {
	info, body, err := readArtifact(path)
	if err != nil {
		return ArtifactAttestation{}, err
	}
	device, inode, ok := durableArtifactIdentity(info)
	if !ok {
		return ArtifactAttestation{}, errors.New("qwen artifact has no durable filesystem identity")
	}
	digest := sha256.Sum256(body)
	return ArtifactAttestation{
		Path: path, Fingerprint: "sha256:" + fmt.Sprintf("%x", digest[:]), Device: device, Inode: inode,
	}, nil
}

// ArtifactIdentityMatches verifies only the durable file identity. Append-only
// event/input content may legitimately change after launch.
func ArtifactIdentityMatches(attestation ArtifactAttestation) bool {
	info, err := os.Lstat(attestation.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	device, inode, ok := durableArtifactIdentity(info)
	return ok && device == attestation.Device && inode == attestation.Inode
}

func readArtifact(path string) (os.FileInfo, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maximumArtifactBytes {
		return nil, nil, errors.New("qwen artifact is not a bounded regular file")
	}
	body, err := os.ReadFile(path) //nolint:gosec // The exact bounded regular path was validated above.
	if err != nil {
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) || current.Size() != int64(len(body)) {
		return nil, nil, errors.New("qwen artifact changed during attestation")
	}
	return current, body, nil
}
