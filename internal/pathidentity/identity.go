package pathidentity

import (
	"fmt"
	"os"
)

// Kind classifies the no-follow filesystem object at an identity path.
type Kind string

const (
	// KindDirectory identifies a real directory.
	KindDirectory Kind = "directory"
	// KindRegular identifies a regular file.
	KindRegular Kind = "regular"
	// KindSocket identifies a Unix socket.
	KindSocket Kind = "socket"
	// KindOther identifies any other non-symlink filesystem object.
	KindOther Kind = "other"
)

// Identity is the canonical no-follow path, type, and mode of one existing object.
type Identity struct {
	Path string
	Kind Kind
	Mode os.FileMode
}

// ExistingNoFollow returns one existing object's canonical identity while
// rejecting every mutable symlink component, including the leaf. Fixed
// platform aliases are handled by FuturePath before the final lstat.
func ExistingNoFollow(value string) (Identity, error) {
	canonical, err := FuturePath(value)
	if err != nil {
		return Identity{}, err
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return Identity{}, fmt.Errorf("inspect no-follow path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Identity{}, fmt.Errorf("path contains mutable symlink component %q", canonical)
	}
	kind := KindOther
	switch {
	case info.IsDir():
		kind = KindDirectory
	case info.Mode().IsRegular():
		kind = KindRegular
	case info.Mode()&os.ModeSocket != 0:
		kind = KindSocket
	}
	return Identity{Path: canonical, Kind: kind, Mode: info.Mode()}, nil
}
