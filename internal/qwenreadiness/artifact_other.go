//go:build !linux && !darwin

package qwenreadiness

import "os"

func durableArtifactIdentity(os.FileInfo) (uint64, uint64, bool) { return 0, 0, false }
