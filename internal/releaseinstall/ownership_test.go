package releaseinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func digestForTest(value string) NativeDigest {
	digest := sha256.Sum256([]byte(value))
	return NativeDigest(hex.EncodeToString(digest[:]))
}

func bytesContainsFold(body []byte, value string) bool {
	return bytes.Contains(bytes.ToLower(body), []byte(value))
}

func TestOwnershipStoreRoundTripModesAndSecretFreeEncoding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := OpenOwnershipStore(root, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	ledger := OwnershipLedger{Schema: OwnershipLedgerSchemaV1, Revision: 1, Receipts: []OwnershipReceipt{{
		ProductID: "codex", Strategy: "legacy-native-plugin", TransactionID: "txn-1", ReleaseID: "release-1",
		Prior:     &NativeIdentity{ResourceKey: "connector", Kind: "plugin", Revision: "old", Digest: digestForTest("old")},
		Installed: NativeIdentity{ResourceKey: "connector", Kind: "plugin", Revision: "new", Digest: digestForTest("new")},
	}}}
	if err := store.WriteLedger(ledger); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadLedger()
	if err != nil || len(got.Receipts) != 1 || got.Receipts[0].Prior.Revision != "old" {
		t.Fatalf("ledger = %#v, %v", got, err)
	}
	rootInfo, _ := os.Stat(filepath.Join(root, ownershipDirectoryName))
	fileInfo, _ := os.Stat(filepath.Join(root, ownershipDirectoryName, ownershipFilename))
	if rootInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes = %o/%o", rootInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	body, _ := os.ReadFile(filepath.Join(root, ownershipDirectoryName, ownershipFilename))
	for _, forbidden := range []string{"password", "credential", "endpoint", "argv", "environment", "--token"} {
		if bytesContainsFold(body, forbidden) {
			t.Fatalf("ownership ledger contains forbidden field/value %q: %s", forbidden, body)
		}
	}
}

func TestOwnershipStoreCanonicalizesReceiptOrderDeterministically(t *testing.T) {
	makeReceipt := func(product string) OwnershipReceipt {
		return OwnershipReceipt{ProductID: product, Strategy: "native-plugin", TransactionID: "txn", ReleaseID: "release", Installed: NativeIdentity{ResourceKey: NativeToken(product), Kind: "plugin", Revision: "one", Digest: digestForTest(product)}}
	}
	write := func(receipts []OwnershipReceipt) []byte {
		root := filepath.Join(t.TempDir(), "state")
		store, err := OpenOwnershipStore(root, 64<<10)
		if err != nil {
			t.Fatal(err)
		}
		ledger := OwnershipLedger{Schema: OwnershipLedgerSchemaV1, Revision: 1, Receipts: receipts}
		if err := store.WriteLedger(ledger); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(root, ownershipDirectoryName, ownershipFilename))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := write([]OwnershipReceipt{makeReceipt("codex"), makeReceipt("claude")})
	second := write([]OwnershipReceipt{makeReceipt("claude"), makeReceipt("codex")})
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical ownership encoding drifted:\n%s\n%s", first, second)
	}
}

func TestOwnershipStoreFailsClosedOnTamperUnknownSchemaOversizeAndSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := OpenOwnershipStore(root, 4096)
	if err != nil {
		t.Fatal(err)
	}
	journal := CrashJournal{Schema: CrashJournalSchemaV1, Revision: 1, TransactionID: "txn", Phase: JournalApplying}
	if err := store.BeginJournal(journal); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ownershipDirectoryName, journalFilename)
	body, _ := os.ReadFile(path)
	body[len(body)/2] ^= 1
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadJournal(); err == nil {
		t.Fatal("tampered journal was accepted")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":"agent-sessions.releaseinstall-journal.v999","revision":1,"transaction_id":"txn","phase":"applying","entries":[],"integrity":"00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadJournal(); err == nil {
		t.Fatal("unknown journal schema was accepted")
	}

	if err := os.WriteFile(path, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadJournal(); !errors.Is(err, ErrOwnershipTooLarge) {
		t.Fatalf("oversize journal error = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadJournal(); err == nil {
		t.Fatal("journal symlink was accepted")
	}
}

func TestOwnershipValidationRejectsZeroRevisionDuplicateReceiptAndSecretShapedIdentity(t *testing.T) {
	identity := NativeIdentity{ResourceKey: "connector", Kind: "plugin", Revision: "one", Digest: digestForTest("one")}
	receipt := OwnershipReceipt{ProductID: "codex", Strategy: "native-plugin", TransactionID: "txn", ReleaseID: "release", Installed: identity}
	store, err := OpenOwnershipStore(filepath.Join(t.TempDir(), "state"), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteLedger(OwnershipLedger{Schema: OwnershipLedgerSchemaV1, Receipts: []OwnershipReceipt{receipt}}); err == nil {
		t.Fatal("zero-revision ledger was accepted")
	}
	if err := store.WriteLedger(OwnershipLedger{Schema: OwnershipLedgerSchemaV1, Revision: 1, Receipts: []OwnershipReceipt{receipt, receipt}}); err == nil {
		t.Fatal("duplicate product-strategy receipt was accepted")
	}
	receipt.Installed.Revision = "bearer-secret"
	if _, err := json.Marshal(receipt.Installed); err == nil {
		t.Fatal("secret-shaped native identity serialized outside the ownership store")
	}
	if err := store.WriteLedger(OwnershipLedger{Schema: OwnershipLedgerSchemaV1, Revision: 1, Receipts: []OwnershipReceipt{receipt}}); err == nil {
		t.Fatal("secret-shaped native identity was accepted")
	}
	journal := CrashJournal{Schema: CrashJournalSchemaV1, TransactionID: "txn", Phase: JournalApplying, Entries: []JournalEntry{{ProductID: "codex", Strategy: "native-plugin", State: JournalEntryPrepared, Planned: &identity}}}
	if err := store.BeginJournal(journal); err == nil {
		t.Fatal("zero-revision journal was accepted")
	}
	journal.Revision = 1
	journal.Entries = append(journal.Entries, journal.Entries[0])
	if err := store.BeginJournal(journal); err == nil {
		t.Fatal("duplicate product-strategy journal entries were accepted")
	}
}

func TestOwnershipStoreCleansOnlyBoundedOwnedCrashTemporaries(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	root := filepath.Join(stateRoot, ownershipDirectoryName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".ownership-stale")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOwnershipStore(stateRoot, 64<<10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned stale temporary survived: %v", err)
	}

	stateRoot = filepath.Join(t.TempDir(), "state")
	root = filepath.Join(stateRoot, ownershipDirectoryName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside", filepath.Join(root, ".ownership-trap")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOwnershipStore(stateRoot, 64<<10); err == nil {
		t.Fatal("stale temporary symlink was removed or accepted")
	}

	stateRoot = filepath.Join(t.TempDir(), "state")
	root = filepath.Join(stateRoot, ownershipDirectoryName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxStaleTemporaries; index++ {
		path := filepath.Join(root, fmt.Sprintf(".ownership-%02d", index))
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenOwnershipStore(stateRoot, 64<<10); err == nil {
		t.Fatal("unbounded stale temporary set was accepted")
	}
}

func TestOwnershipTransactionAdmissionSerializesWholeOperation(t *testing.T) {
	store, err := OpenOwnershipStore(filepath.Join(t.TempDir(), "state"), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	enteredFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.withTransaction(func() error {
			close(enteredFirst)
			<-releaseFirst
			return nil
		})
	}()
	<-enteredFirst
	secondStarted := make(chan struct{})
	enteredSecond := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- store.withTransaction(func() error { close(enteredSecond); return nil })
	}()
	<-secondStarted
	select {
	case <-enteredSecond:
		t.Fatal("second transaction entered while first held admission")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-enteredSecond:
	case <-time.After(time.Second):
		t.Fatal("second transaction did not enter after release")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestOwnershipTransactionLockFailsClosedAfterSymlinkReplacement(t *testing.T) {
	store, err := OpenOwnershipStore(filepath.Join(t.TempDir(), "state"), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.root, transactionLockFilename)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ownershipLockFilename, lockPath); err != nil {
		t.Fatal(err)
	}
	if err := store.withTransaction(func() error { return nil }); err == nil {
		t.Fatal("transaction lock symlink was accepted")
	}
}
