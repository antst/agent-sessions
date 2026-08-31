// Package releasepkg creates byte-stable Agent Sessions release archives.
package releasepkg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Create writes a deterministic gzip-compressed tar archive containing the
// named package directory beneath stageRoot.
//
//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func Create(stageRoot, packageName, destination string) error {
	if filepath.Base(packageName) != packageName || packageName == "." || packageName == "" {
		return errors.New("release package name must be one safe path component")
	}
	root := filepath.Join(stageRoot, packageName)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("release package root is not a real directory")
	}
	entries := make([]string, 0, 256)
	err = filepath.Walk(root, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries = append(entries, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk release package: %w", err)
	}
	sort.Strings(entries)

	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".release-archive-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	gzipWriter, err := gzip.NewWriterLevel(temporary, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	for _, path := range entries {
		if err := writeEntry(tarWriter, stageRoot, path); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	committed = true
	return nil
}

//nolint:gocyclo // Explicit validation and lifecycle gates remain together for fail-closed auditability.
func writeEntry(writer *tar.Writer, stageRoot, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(stageRoot, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid release entry %q", path)
	}
	name := filepath.ToSlash(relative)
	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return err
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = name
	header.Uid, header.Gid = 0, 0
	header.Uname, header.Gname = "", ""
	header.ModTime = time.Unix(0, 0).UTC()
	header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
	// PAX supports the repository's long documentation paths. archive/tar
	// serializes PAX records deterministically from this normalized header.
	header.Format = tar.FormatPAX
	header.PAXRecords = nil
	switch {
	case info.IsDir():
		header.Mode = 0o755
		header.Name += "/"
	case info.Mode().IsRegular() && info.Mode()&0o111 != 0:
		header.Mode = 0o755
	case info.Mode().IsRegular():
		header.Mode = 0o644
	case info.Mode()&os.ModeSymlink != 0:
		header.Mode = 0o777
	default:
		return fmt.Errorf("unsupported release entry type %q", path)
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path) //nolint:gosec // path comes from walking the validated private staging root.
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(writer, file)
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}
