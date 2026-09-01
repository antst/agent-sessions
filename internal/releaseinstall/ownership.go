package releaseinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/antst/agent-sessions/internal/pathidentity"
	"github.com/antst/agent-sessions/internal/productcatalog"
)

const (
	OwnershipLedgerSchemaV1 = "agent-sessions.releaseinstall-ownership.v1"
	CrashJournalSchemaV1    = "agent-sessions.releaseinstall-journal.v1"
	ownershipDirectoryName  = "release-install"
	ownershipFilename       = "ownership.json"
	journalFilename         = "journal.json"
	ownershipLockFilename   = "ownership.lock"
	transactionLockFilename = "transaction.lock"
	maxStaleTemporaries     = 32
)

var ErrOwnershipTooLarge = errors.New("release install state exceeds configured bound")

type OwnershipReceipt struct {
	ProductID     string          `json:"product_id"`
	Strategy      string          `json:"strategy"`
	TransactionID string          `json:"transaction_id"`
	ReleaseID     string          `json:"release_id"`
	Prior         *NativeIdentity `json:"prior,omitempty"`
	Installed     NativeIdentity  `json:"installed"`
	Debt          string          `json:"debt,omitempty"`
}

type OwnershipLedger struct {
	Schema    string             `json:"schema"`
	Revision  uint64             `json:"revision"`
	Receipts  []OwnershipReceipt `json:"receipts"`
	Integrity string             `json:"integrity,omitempty"`
}

type JournalPhase string

const (
	JournalApplying    JournalPhase = "applying"
	JournalRollingBack JournalPhase = "rolling-back"
	JournalRemoving    JournalPhase = "removing"
)

type JournalEntryState string

const (
	JournalEntryPrepared   JournalEntryState = "prepared"
	JournalEntryRegistered JournalEntryState = "registered"
	JournalEntryVerified   JournalEntryState = "verified"
	JournalEntryDebt       JournalEntryState = "debt"
)

type JournalEntry struct {
	ProductID       string            `json:"product_id"`
	Strategy        string            `json:"strategy"`
	State           JournalEntryState `json:"state"`
	Prior           *NativeIdentity   `json:"prior,omitempty"`
	Planned         *NativeIdentity   `json:"planned,omitempty"`
	Installed       *NativeIdentity   `json:"installed,omitempty"`
	CleanupRequired bool              `json:"cleanup_required,omitempty"`
	Debt            string            `json:"debt,omitempty"`
}

type CrashJournal struct {
	Schema        string         `json:"schema"`
	Revision      uint64         `json:"revision"`
	TransactionID string         `json:"transaction_id"`
	Phase         JournalPhase   `json:"phase"`
	Entries       []JournalEntry `json:"entries"`
	Integrity     string         `json:"integrity,omitempty"`
}

type OwnershipStore struct {
	root          string
	maxBytes      int64
	beforeReplace func(string) error
	afterReplace  func(string) error
}

func OpenOwnershipStore(stateRoot string, maxBytes int64) (*OwnershipStore, error) {
	if maxBytes <= 0 {
		return nil, errors.New("ownership state bound must be positive")
	}
	canonical, err := pathidentity.FuturePath(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve ownership state root: %w", err)
	}
	root := filepath.Join(canonical, ownershipDirectoryName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create ownership state root: %w", err)
	}
	identity, err := pathidentity.ExistingNoFollow(root)
	if err != nil || identity.Kind != pathidentity.KindDirectory {
		return nil, errors.New("ownership state root must be a no-follow directory")
	}
	if identity.Mode.Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			return nil, fmt.Errorf("secure ownership state root: %w", err)
		}
	}
	for _, name := range []string{ownershipLockFilename, transactionLockFilename} {
		if err := createPrivateLock(filepath.Join(root, name)); err != nil {
			return nil, err
		}
	}
	store := &OwnershipStore{root: root, maxBytes: maxBytes}
	if err := store.withTransaction(store.cleanupStaleTemporaries); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *OwnershipStore) ReadLedger() (OwnershipLedger, error) {
	var result OwnershipLedger
	err := store.withLock(func() error { return store.readJSON(ownershipFilename, &result) })
	if err != nil || result.Schema == "" {
		return result, err
	}
	if result.Schema != OwnershipLedgerSchemaV1 {
		return OwnershipLedger{}, fmt.Errorf("unknown ownership ledger schema %q", result.Schema)
	}
	if err := verifyIntegrity(&result); err != nil {
		return OwnershipLedger{}, err
	}
	if err := validateLedger(result); err != nil {
		return OwnershipLedger{}, err
	}
	return cloneLedger(result), nil
}

func (store *OwnershipStore) WriteLedger(ledger OwnershipLedger) error {
	ledger = cloneLedger(ledger)
	if ledger.Schema != OwnershipLedgerSchemaV1 || ledger.Revision == 0 {
		return errors.New("ownership ledger identity is invalid")
	}
	sort.Slice(ledger.Receipts, func(i, j int) bool {
		if ledger.Receipts[i].ProductID == ledger.Receipts[j].ProductID {
			return ledger.Receipts[i].Strategy < ledger.Receipts[j].Strategy
		}
		return ledger.Receipts[i].ProductID < ledger.Receipts[j].ProductID
	})
	if err := validateLedger(ledger); err != nil {
		return err
	}
	if err := setIntegrity(&ledger); err != nil {
		return err
	}
	return store.withLock(func() error {
		var current OwnershipLedger
		if err := store.readJSON(ownershipFilename, &current); err != nil {
			return err
		}
		if current.Schema == "" {
			if ledger.Revision != 1 {
				return errors.New("initial ownership ledger revision must be one")
			}
		} else {
			if current.Schema != OwnershipLedgerSchemaV1 || verifyIntegrity(&current) != nil || validateLedger(current) != nil {
				return errors.New("existing ownership ledger is invalid")
			}
			if ledger.Revision != current.Revision+1 {
				return fmt.Errorf("ownership ledger revision conflict: current=%d proposed=%d", current.Revision, ledger.Revision)
			}
		}
		return store.writeJSON(ownershipFilename, ledger)
	})
}

// RestoreLedgerAfterAmbiguousWrite atomically accepts only the exact prior or
// candidate snapshot. If the candidate rename landed, it restores the prior
// contents with a fresh monotonic revision (or removes an originally absent
// ledger). Any third state is debt and remains journal-gated.
func (store *OwnershipStore) RestoreLedgerAfterAmbiguousWrite(prior, candidate OwnershipLedger) error {
	prior, candidate = cloneLedger(prior), cloneLedger(candidate)
	return store.withLock(func() error {
		var current OwnershipLedger
		if err := store.readJSON(ownershipFilename, &current); err != nil {
			return err
		}
		if current.Schema != "" {
			if current.Schema != OwnershipLedgerSchemaV1 || verifyIntegrity(&current) != nil || validateLedger(current) != nil {
				return errors.New("live ownership ledger is invalid during restoration")
			}
		}
		if ledgerSameSnapshot(current, prior) {
			return nil
		}
		if !ledgerSameSnapshot(current, candidate) {
			return ErrInstallDebt
		}
		if prior.Schema == "" {
			path := filepath.Join(store.root, ownershipFilename)
			if err := os.Remove(path); err != nil {
				return err
			}
			return syncDirectory(store.root)
		}
		restored := cloneLedger(prior)
		restored.Revision = current.Revision + 1
		if err := validateLedger(restored); err != nil {
			return err
		}
		if err := setIntegrity(&restored); err != nil {
			return err
		}
		return store.writeJSON(ownershipFilename, restored)
	})
}

func ledgerSameSnapshot(left, right OwnershipLedger) bool {
	left.Integrity, right.Integrity = "", ""
	return reflect.DeepEqual(left, right)
}

func ledgerSameContents(left, right OwnershipLedger) bool {
	left.Integrity, right.Integrity = "", ""
	left.Revision, right.Revision = 0, 0
	return reflect.DeepEqual(left, right)
}

func (store *OwnershipStore) ReadJournal() (CrashJournal, error) {
	var result CrashJournal
	err := store.withLock(func() error { return store.readJSON(journalFilename, &result) })
	if err != nil || result.Schema == "" {
		return result, err
	}
	if result.Schema != CrashJournalSchemaV1 {
		return CrashJournal{}, fmt.Errorf("unknown crash journal schema %q", result.Schema)
	}
	if err := verifyIntegrity(&result); err != nil {
		return CrashJournal{}, err
	}
	if err := validateJournal(result); err != nil {
		return CrashJournal{}, err
	}
	return cloneJournal(result), nil
}

func (store *OwnershipStore) WriteJournal(journal CrashJournal) error {
	journal = cloneJournal(journal)
	if journal.Schema != CrashJournalSchemaV1 || journal.Revision == 0 {
		return errors.New("crash journal identity is invalid")
	}
	if err := validateJournal(journal); err != nil {
		return err
	}
	if err := setIntegrity(&journal); err != nil {
		return err
	}
	return store.withLock(func() error {
		var current CrashJournal
		if err := store.readJSON(journalFilename, &current); err != nil {
			return err
		}
		if current.Schema == "" {
			return errors.New("crash journal must be created with BeginJournal")
		}
		if current.Schema != CrashJournalSchemaV1 || verifyIntegrity(&current) != nil {
			return errors.New("existing crash journal is invalid")
		}
		if current.TransactionID != journal.TransactionID || journal.Revision <= current.Revision {
			return ErrInstallInProgress
		}
		return store.writeJSON(journalFilename, journal)
	})
}

// BeginJournal creates the one crash journal only when no prior transaction
// needs recovery. The absence check and durable write share the store lock.
func (store *OwnershipStore) BeginJournal(journal CrashJournal) error {
	journal = cloneJournal(journal)
	if journal.Schema != CrashJournalSchemaV1 || journal.Revision != 1 {
		return errors.New("initial crash journal identity is invalid")
	}
	if err := validateJournal(journal); err != nil {
		return err
	}
	if err := setIntegrity(&journal); err != nil {
		return err
	}
	return store.withLock(func() error {
		path := filepath.Join(store.root, journalFilename)
		if _, err := os.Lstat(path); err == nil {
			return ErrInstallInProgress
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return store.writeJSON(journalFilename, journal)
	})
}

func (store *OwnershipStore) ClearJournal() error {
	return store.withLock(func() error {
		path := filepath.Join(store.root, journalFilename)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("crash journal is not a regular no-follow file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return syncDirectory(store.root)
	})
}

func (store *OwnershipStore) withLock(operation func() error) (returnErr error) {
	fd, err := openPrivateLock(filepath.Join(store.root, ownershipLockFilename))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, unix.Close(fd)) }()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	return operation()
}

func (store *OwnershipStore) withTransaction(operation func() error) (returnErr error) {
	fd, err := openPrivateLock(filepath.Join(store.root, transactionLockFilename))
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, unix.Close(fd)) }()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(fd, unix.LOCK_UN) }()
	return operation()
}

func openPrivateLock(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 {
		_ = unix.Close(fd)
		return -1, errors.New("release install lock is not a 0600 regular file")
	}
	return fd, nil
}

func createPrivateLock(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open release install lock: %w", err)
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("secure release install lock: %w", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return errors.New("release install lock is not a regular file")
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close release install lock: %w", err)
	}
	return nil
}

func (store *OwnershipStore) cleanupStaleTemporaries() error {
	entries, err := os.ReadDir(store.root)
	if err != nil {
		return err
	}
	removed := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".ownership-") {
			continue
		}
		removed++
		if removed > maxStaleTemporaries {
			return errors.New("too many stale release install temporary files")
		}
		path := filepath.Join(store.root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			return errors.New("stale release install temporary is not an owned 0600 regular file")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if removed > 0 {
		return syncDirectory(store.root)
	}
	return nil
}

func (store *OwnershipStore) readJSON(name string, target any) error {
	path := filepath.Join(store.root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a regular no-follow file", name)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s must have mode 0600", name)
	}
	if info.Size() <= 0 || info.Size() > store.maxBytes {
		return ErrOwnershipTooLarge
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	var pathStat, descriptorStat unix.Stat_t
	if err := unix.Lstat(path, &pathStat); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if err := unix.Fstat(fd, &descriptorStat); err != nil {
		_ = unix.Close(fd)
		return err
	}
	if pathStat.Dev != descriptorStat.Dev || pathStat.Ino != descriptorStat.Ino || descriptorStat.Mode&unix.S_IFMT != unix.S_IFREG || descriptorStat.Mode&0o777 != 0o600 {
		_ = unix.Close(fd)
		return fmt.Errorf("%s changed identity while opening", name)
	}
	file := os.NewFile(uintptr(fd), path)
	body, readErr := io.ReadAll(io.LimitReader(file, store.maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if int64(len(body)) > store.maxBytes {
		return ErrOwnershipTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("release install state contains trailing data")
	}
	return nil
}

func (store *OwnershipStore) writeJSON(name string, value any) (returnErr error) {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if int64(len(body)) > store.maxBytes {
		return ErrOwnershipTooLarge
	}
	destination := filepath.Join(store.root, name)
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is not a regular no-follow file", name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(store.root, ".ownership-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if store.beforeReplace != nil {
		if err := store.beforeReplace(name); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	if store.afterReplace != nil {
		if err := store.afterReplace(name); err != nil {
			return err
		}
	}
	return syncDirectory(store.root)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func setIntegrity(value any) error {
	switch typed := value.(type) {
	case *OwnershipLedger:
		typed.Integrity = ""
		body, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		typed.Integrity = hex.EncodeToString(digest[:])
	case *CrashJournal:
		typed.Integrity = ""
		body, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		typed.Integrity = hex.EncodeToString(digest[:])
	default:
		return errors.New("unsupported integrity record")
	}
	return nil
}

func verifyIntegrity(value any) error {
	var got string
	switch typed := value.(type) {
	case *OwnershipLedger:
		got, typed.Integrity = typed.Integrity, ""
	case *CrashJournal:
		got, typed.Integrity = typed.Integrity, ""
	default:
		return errors.New("unsupported integrity record")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if len(got) != sha256.Size*2 || !strings.EqualFold(got, hex.EncodeToString(digest[:])) {
		return errors.New("release install state integrity mismatch")
	}
	return nil
}

func validateLedger(ledger OwnershipLedger) error {
	if ledger.Revision == 0 {
		return errors.New("ownership ledger revision must be positive")
	}
	seen := map[string]bool{}
	for index := range ledger.Receipts {
		receipt := &ledger.Receipts[index]
		if err := productcatalog.ValidateToken(receipt.ProductID); err != nil {
			return err
		}
		if err := productcatalog.ValidateToken(receipt.Strategy); err != nil {
			return err
		}
		if err := validateBounded(receipt.TransactionID, 128); err != nil {
			return err
		}
		if err := validateBounded(receipt.ReleaseID, 128); err != nil {
			return err
		}
		if err := validateIdentity(receipt.Installed); err != nil {
			return err
		}
		if receipt.Prior != nil {
			if err := validateIdentity(*receipt.Prior); err != nil {
				return err
			}
		}
		if receipt.Debt != "" {
			if err := productcatalog.ValidateToken(receipt.Debt); err != nil {
				return err
			}
		}
		key := receipt.ProductID + "\x00" + receipt.Strategy
		if seen[key] {
			return errors.New("duplicate ownership receipt")
		}
		seen[key] = true
	}
	return nil
}

func validateJournal(journal CrashJournal) error {
	if journal.Revision == 0 {
		return errors.New("crash journal revision must be positive")
	}
	if err := validateBounded(journal.TransactionID, 128); err != nil {
		return err
	}
	if journal.Phase != JournalApplying && journal.Phase != JournalRollingBack && journal.Phase != JournalRemoving {
		return errors.New("invalid crash journal phase")
	}
	seen := map[string]bool{}
	for index := range journal.Entries {
		entry := &journal.Entries[index]
		if err := productcatalog.ValidateToken(entry.ProductID); err != nil {
			return err
		}
		if err := productcatalog.ValidateToken(entry.Strategy); err != nil {
			return err
		}
		key := entry.ProductID + "\x00" + entry.Strategy
		if seen[key] {
			return errors.New("duplicate crash journal entry")
		}
		seen[key] = true
		switch entry.State {
		case JournalEntryPrepared, JournalEntryRegistered, JournalEntryVerified, JournalEntryDebt:
		default:
			return errors.New("invalid crash journal entry state")
		}
		if entry.Prior != nil {
			if err := validateIdentity(*entry.Prior); err != nil {
				return err
			}
		}
		if entry.Installed != nil {
			if err := validateIdentity(*entry.Installed); err != nil {
				return err
			}
		}
		if entry.Planned != nil {
			if err := validateIdentity(*entry.Planned); err != nil {
				return err
			}
		}
		if journal.Phase != JournalRemoving && entry.Planned == nil {
			return errors.New("apply journal entry requires planned identity")
		}
		if entry.CleanupRequired && entry.Planned == nil {
			return errors.New("cleanup obligation requires planned identity")
		}
		if entry.State == JournalEntryPrepared && entry.Installed != nil && journal.Phase != JournalRemoving {
			return errors.New("prepared journal entry must not claim an installed identity")
		}
		if (entry.State == JournalEntryRegistered || entry.State == JournalEntryVerified) && entry.Installed == nil {
			return errors.New("registered journal entry requires an installed identity")
		}
		if entry.Debt != "" {
			if err := productcatalog.ValidateToken(entry.Debt); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateIdentity(identity NativeIdentity) error {
	if err := productcatalog.ValidateToken(string(identity.ResourceKey)); err != nil {
		return fmt.Errorf("resource key must be a product token: %w", err)
	}
	if err := productcatalog.ValidateToken(string(identity.Kind)); err != nil {
		return fmt.Errorf("identity kind must be a product token: %w", err)
	}
	if err := validateNativeRevision(string(identity.Revision)); err != nil {
		return err
	}
	if len(identity.Digest) != sha256.Size*2 {
		return errors.New("identity digest must be sha256 hex")
	}
	if _, err := hex.DecodeString(string(identity.Digest)); err != nil {
		return errors.New("identity digest must be sha256 hex")
	}
	return nil
}

func validateNativeRevision(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("native revision is empty or oversized")
	}
	lower := strings.ToLower(value)
	for _, forbidden := range []string{"token", "secret", "password", "credential", "bearer"} {
		if strings.Contains(lower, forbidden) {
			return errors.New("native revision contains secret-shaped content")
		}
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '+' || character == '-' {
			continue
		}
		return fmt.Errorf("native revision contains unsafe character %q", character)
	}
	return nil
}

func validateBounded(value string, max int) error {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return errors.New("value is empty, oversized, or invalid utf-8")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("value contains control characters")
		}
	}
	return nil
}

func cloneLedger(ledger OwnershipLedger) OwnershipLedger {
	result := ledger
	result.Receipts = append([]OwnershipReceipt(nil), ledger.Receipts...)
	for index := range result.Receipts {
		result.Receipts[index].Prior = cloneNativeIdentity(result.Receipts[index].Prior)
	}
	return result
}

func cloneJournal(journal CrashJournal) CrashJournal {
	result := journal
	result.Entries = append([]JournalEntry(nil), journal.Entries...)
	for index := range result.Entries {
		result.Entries[index].Prior = cloneNativeIdentity(result.Entries[index].Prior)
		result.Entries[index].Planned = cloneNativeIdentity(result.Entries[index].Planned)
		result.Entries[index].Installed = cloneNativeIdentity(result.Entries[index].Installed)
	}
	return result
}
