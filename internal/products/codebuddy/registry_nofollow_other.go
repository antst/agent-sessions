//go:build !linux && !darwin

package codebuddy

import (
	"errors"
	"os"
)

func readNoFollowAt(*os.File, string, int64) ([]byte, os.FileInfo, error) {
	return nil, nil, errors.New("secure codebuddy registry reads are unsupported on this platform")
}

func fileIdentity(os.FileInfo) (uint64, uint64, bool) { return 0, 0, false }
