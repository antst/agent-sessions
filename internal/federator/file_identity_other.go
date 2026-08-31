//go:build !linux && !darwin

package federator

import "os"

func durableFileIdentity(os.FileInfo) (uint64, uint64, bool) {
	return 0, 0, false
}
