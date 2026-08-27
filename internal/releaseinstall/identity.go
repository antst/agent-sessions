package releaseinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// ContentIdentity hashes the exact bounded release tree except manifest.json,
// whose content_identity field necessarily describes that tree. Filesystem
// types, relative paths, permissions, sizes, and regular-file bytes are bound.
//
//nolint:gocyclo // The walk deliberately validates every supported type while streaming one exact identity.
func ContentIdentity(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("release identity root is not a real directory")
	}
	hash := sha256.New()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || relative == "manifest.json" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("release identity contains unsupported entry %s", relative)
		}
		kind := "d"
		if info.Mode().IsRegular() {
			kind = "f"
		}
		_, _ = io.WriteString(hash, kind+"\x00"+filepath.ToSlash(relative)+"\x00"+
			strconv.FormatUint(uint64(info.Mode().Perm()), 8)+"\x00"+strconv.FormatInt(info.Size(), 10)+"\x00")
		if info.Mode().IsRegular() {
			file, err := os.Open(path) //nolint:gosec // WalkDir selected a regular entry below the exact release root.
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		_, _ = io.WriteString(hash, "\x00")
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
