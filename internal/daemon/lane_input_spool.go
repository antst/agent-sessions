package daemon

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antst/agent-sessions/internal/pathidentity"
	"golang.org/x/sys/unix"
)

const (
	laneInputSpoolPrefix = "spool-"
	laneInputTempPrefix  = ".admit-"
)

type laneInputSpool struct {
	root         string
	rootIdentity laneInputObjectIdentity
}

type laneInputObjectIdentity struct {
	device uint64
	inode  uint64
}

func openLaneInputSpool(root string) (*laneInputSpool, error) {
	canonical, err := pathidentity.FuturePath(root)
	if err != nil {
		return nil, fmt.Errorf("resolve lane input spool root: %w", err)
	}
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		return nil, fmt.Errorf("create lane input spool root: %w", err)
	}
	identity, err := pathidentity.ExistingNoFollow(canonical)
	if err != nil {
		return nil, fmt.Errorf("verify lane input spool root: %w", err)
	}
	if identity.Kind != pathidentity.KindDirectory || identity.Mode.Perm() != 0o700 {
		return nil, errors.New("lane input spool root must be a no-follow 0700 directory")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(canonical, &stat); err != nil {
		return nil, fmt.Errorf("inspect lane input spool owner: %w", err)
	}
	if uint32(stat.Uid) != uint32(os.Geteuid()) {
		return nil, errors.New("lane input spool root is not owned by the current user")
	}
	return &laneInputSpool{root: canonical, rootIdentity: laneInputIdentity(stat)}, nil
}

func (s *laneInputSpool) create(randomID string, body []byte) (string, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if err := s.validateRoot(); err != nil {
		return "", digest, err
	}
	if !validDurableOpaqueID(randomID) {
		return "", digest, errors.New("lane input random identifier is invalid")
	}
	temporaryName := laneInputTempPrefix + randomID
	temporaryPath := filepath.Join(s.root, temporaryName)
	fd, err := unix.Open(temporaryPath, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", digest, fmt.Errorf("create exclusive lane input spool object: %w", err)
	}
	file := os.NewFile(uintptr(fd), temporaryPath)
	keepTemporary := true
	defer func() {
		_ = file.Close()
		if keepTemporary {
			_ = unix.Unlink(temporaryPath)
		}
	}()

	written, err := io.Copy(file, bytes.NewReader(body))
	if err != nil {
		return "", digest, fmt.Errorf("write lane input spool object: wrote=%d: %w", written, err)
	}
	if written != int64(len(body)) {
		return "", digest, fmt.Errorf("short lane input spool write: wrote=%d want=%d", written, len(body))
	}
	if err := file.Sync(); err != nil {
		return "", digest, fmt.Errorf("sync lane input spool object: %w", err)
	}
	stat, err := statLaneInputFile(file)
	if err != nil {
		return "", digest, err
	}
	if stat.size != int64(len(body)) {
		return "", digest, errors.New("lane input spool size changed during admission")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", digest, fmt.Errorf("rewind admitted lane input spool object: %w", err)
	}
	hasher := sha256.New()
	verifiedBytes, err := io.Copy(hasher, io.LimitReader(file, int64(len(body))+1))
	if err != nil {
		return "", digest, fmt.Errorf("verify admitted lane input spool object: read=%d: %w", verifiedBytes, err)
	}
	if verifiedBytes != int64(len(body)) {
		return "", digest, fmt.Errorf("short admitted lane input verification: read=%d want=%d", verifiedBytes, len(body))
	}
	copy(digest[:], hasher.Sum(nil))
	wantDigest := sha256.Sum256(body)
	if digest != wantDigest {
		return "", digest, errors.New("lane input spool digest changed during admission")
	}
	objectID := fmt.Sprintf("%s%s-%x-%x", laneInputSpoolPrefix, randomID, stat.identity.device, stat.identity.inode)
	if !validDurableOpaqueID(objectID) {
		return "", digest, errors.New("lane input spool object identifier is invalid")
	}
	objectPath := filepath.Join(s.root, objectID)
	if err := unix.Link(temporaryPath, objectPath); err != nil {
		return "", digest, fmt.Errorf("publish exclusive lane input spool object: %w", err)
	}
	if err := unix.Unlink(temporaryPath); err != nil {
		_ = unix.Unlink(objectPath)
		return "", digest, fmt.Errorf("remove lane input spool temporary object: %w", err)
	}
	keepTemporary = false
	if err := s.syncRoot(); err != nil {
		return "", digest, err
	}
	return objectID, digest, nil
}

type verifiedLaneInput struct {
	file     *os.File
	size     int64
	digest   [sha256.Size]byte
	identity laneInputObjectIdentity
}

func (s *laneInputSpool) openVerified(receipt LaneInputReceipt) (verifiedLaneInput, error) {
	if err := s.validateRoot(); err != nil {
		return verifiedLaneInput{}, err
	}
	wantIdentity, err := parseLaneInputObjectIdentity(receipt.SpoolObjectID)
	if err != nil {
		return verifiedLaneInput{}, err
	}
	path := filepath.Join(s.root, receipt.SpoolObjectID)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return verifiedLaneInput{}, fmt.Errorf("open verified lane input: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	verified := verifiedLaneInput{file: file}
	stat, err := statLaneInputFile(file)
	if err != nil {
		_ = file.Close()
		return verifiedLaneInput{}, err
	}
	if stat.identity != wantIdentity || stat.size != receipt.Bytes {
		_ = file.Close()
		return verifiedLaneInput{}, errors.New("lane input spool identity or size changed")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(file, receipt.Bytes+1)); err != nil {
		_ = file.Close()
		return verifiedLaneInput{}, fmt.Errorf("hash lane input spool object: %w", err)
	}
	copy(verified.digest[:], hasher.Sum(nil))
	if verified.digest != receipt.Digest {
		_ = file.Close()
		return verifiedLaneInput{}, errors.New("lane input spool digest changed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return verifiedLaneInput{}, fmt.Errorf("rewind lane input spool object: %w", err)
	}
	verified.size, verified.identity = stat.size, stat.identity
	return verified, nil
}

func (s *laneInputSpool) removeVerified(receipt LaneInputReceipt) error {
	verified, err := s.openVerified(receipt)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := verified.file.Close(); err != nil {
		return fmt.Errorf("close verified lane input spool object: %w", err)
	}
	path := filepath.Join(s.root, receipt.SpoolObjectID)
	var current unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("recheck lane input spool object: %w", err)
	}
	if laneInputIdentity(current) != verified.identity || current.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("lane input spool object changed before removal")
	}
	if err := unix.Unlink(path); err != nil {
		return fmt.Errorf("remove lane input spool object: %w", err)
	}
	return s.syncRoot()
}

type laneInputFileStat struct {
	identity laneInputObjectIdentity
	size     int64
}

func statLaneInputFile(file *os.File) (laneInputFileStat, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return laneInputFileStat{}, fmt.Errorf("inspect lane input spool object: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || uint32(stat.Uid) != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return laneInputFileStat{}, errors.New("lane input spool object is not an exact owned 0600 regular file")
	}
	return laneInputFileStat{identity: laneInputIdentity(stat), size: stat.Size}, nil
}

func laneInputIdentity(stat unix.Stat_t) laneInputObjectIdentity {
	return laneInputObjectIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func parseLaneInputObjectIdentity(objectID string) (laneInputObjectIdentity, error) {
	parts := strings.Split(objectID, "-")
	if len(parts) < 4 || parts[0] != strings.TrimSuffix(laneInputSpoolPrefix, "-") {
		return laneInputObjectIdentity{}, errors.New("lane input spool object identity is malformed")
	}
	device, err := strconv.ParseUint(parts[len(parts)-2], 16, 64)
	if err != nil {
		return laneInputObjectIdentity{}, errors.New("lane input spool device identity is malformed")
	}
	inode, err := strconv.ParseUint(parts[len(parts)-1], 16, 64)
	if err != nil || device == 0 || inode == 0 {
		return laneInputObjectIdentity{}, errors.New("lane input spool inode identity is malformed")
	}
	return laneInputObjectIdentity{device: device, inode: inode}, nil
}

func (s *laneInputSpool) syncRoot() error {
	if err := s.validateRoot(); err != nil {
		return err
	}
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open lane input spool root for sync: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync lane input spool root: %w", err)
	}
	return nil
}

func (s *laneInputSpool) validateRoot() error {
	var stat unix.Stat_t
	if err := unix.Lstat(s.root, &stat); err != nil {
		return fmt.Errorf("revalidate lane input spool root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o777 != 0o700 || uint32(stat.Uid) != uint32(os.Geteuid()) ||
		laneInputIdentity(stat) != s.rootIdentity {
		return errors.New("lane input spool root identity changed")
	}
	return nil
}

func (s *laneInputSpool) removeExactOwnedOrphan(name string) (bool, error) {
	if err := s.validateRoot(); err != nil {
		return false, err
	}
	var want *laneInputObjectIdentity
	if strings.HasPrefix(name, laneInputSpoolPrefix) {
		identity, err := parseLaneInputObjectIdentity(name)
		if err != nil {
			return false, nil
		}
		want = &identity
	} else if strings.HasPrefix(name, laneInputTempPrefix) {
		if !validDurableOpaqueID(strings.TrimPrefix(name, laneInputTempPrefix)) {
			return false, nil
		}
	} else {
		return false, nil
	}
	path := filepath.Join(s.root, name)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true, nil
		}
		return false, err
	}
	file := os.NewFile(uintptr(fd), path)
	stat, statErr := statLaneInputFile(file)
	closeErr := file.Close()
	if statErr != nil {
		return false, nil
	}
	if closeErr != nil {
		return false, closeErr
	}
	if want != nil && stat.identity != *want {
		return false, nil
	}
	var current unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true, nil
		}
		return false, err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG || laneInputIdentity(current) != stat.identity {
		return false, nil
	}
	if err := unix.Unlink(path); err != nil {
		return false, err
	}
	return true, nil
}
