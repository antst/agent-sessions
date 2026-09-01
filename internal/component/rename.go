package component

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
)

// RenameFailureCategory is the component-local machine category returned to
// the composition layer. The central gateway maps these values onto its
// productruntime taxonomy without making component depend on that package.
type RenameFailureCategory string

const (
	RenameUnsupported    RenameFailureCategory = "unsupported"
	RenameUnavailable    RenameFailureCategory = "unavailable"
	RenameTimedOut       RenameFailureCategory = "timed-out"
	RenameNativeRejected RenameFailureCategory = "native-rejected"
	RenameProtocol       RenameFailureCategory = "protocol"
)

// RenameError carries a stable component-local category and redacted detail.
type RenameError struct {
	Category RenameFailureCategory
	Detail   string
}

func (e *RenameError) Error() string {
	if e.Detail == "" {
		return string(e.Category)
	}
	return string(e.Category) + ": " + e.Detail
}

type renameOutcome struct {
	requestDigest   [sha256.Size]byte
	nativeSessionID string
	requestedName   string
	responseDigest  [sha256.Size]byte
	result          SessionRename
	err             *RenameError
	done            chan struct{}
	handling        bool
}

type renameTracker struct {
	mu             sync.Mutex
	maxOutstanding int
	replayWindow   uint64
	pending        map[string]*renameOutcome
	completed      map[string]*renameOutcome
	completedOrder []string
	closed         bool
}

func newRenameTracker(maxOutstanding int, replayWindow uint64) *renameTracker {
	return &renameTracker{
		maxOutstanding: maxOutstanding,
		replayWindow:   replayWindow,
		pending:        make(map[string]*renameOutcome),
		completed:      make(map[string]*renameOutcome),
	}
}

func (r *renameTracker) reserve(operationID string, request SessionRenameRequest) (*renameOutcome, bool, error) {
	digest := renameRequestDigest(request)
	r.mu.Lock()
	defer r.mu.Unlock()
	if completed := r.completed[operationID]; completed != nil {
		if completed.requestDigest != digest {
			return nil, false, renameError(RenameProtocol, "rename operation id conflicts with completed request")
		}
		return completed, false, nil
	}
	if pending := r.pending[operationID]; pending != nil {
		if pending.requestDigest != digest {
			return nil, false, renameError(RenameProtocol, "rename operation id conflicts with outstanding request")
		}
		return pending, false, nil
	}
	if r.closed {
		return nil, false, renameError(RenameUnavailable, "component binding is unavailable")
	}
	if len(r.pending) >= r.maxOutstanding {
		return nil, false, renameError(RenameUnavailable, "too many component rename requests are outstanding")
	}
	operation := &renameOutcome{
		requestDigest: digest, nativeSessionID: request.NativeSessionID,
		requestedName: request.RequestedName, done: make(chan struct{}),
	}
	r.pending[operationID] = operation
	return operation, true, nil
}

func (r *renameTracker) failSend(operationID string, operation *renameOutcome, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[operationID] != operation {
		return
	}
	delete(r.pending, operationID)
	operation.err = renameError(RenameUnavailable, Redact(err.Error()))
	close(operation.done)
	r.rememberLocked(operationID, operation)
}

func (r *renameTracker) admitResponse(frame Frame) (*renameOutcome, bool, error) {
	var response SessionRename
	if err := frame.PayloadInto(&response); err != nil {
		return nil, false, err
	}
	responseDigest := sha256.Sum256(frame.Payload)
	r.mu.Lock()
	defer r.mu.Unlock()
	if completed := r.completed[frame.ID]; completed != nil {
		if completed.responseDigest == responseDigest && completed.err == nil {
			return nil, true, nil
		}
		return nil, false, &ProtocolError{Category: CategoryReplay, Detail: "rename response conflicts with completed native evidence"}
	}
	operation := r.pending[frame.ID]
	if operation == nil {
		return nil, false, &ProtocolError{Category: CategoryProtocol, Detail: "rename response does not name an outstanding daemon request"}
	}
	if response.NativeSessionID != operation.nativeSessionID || response.NativeName != operation.requestedName {
		return operation, false, &ProtocolError{Category: CategoryProtocol, Detail: "rename response does not match the exact requested session and name"}
	}
	operation.result = response
	operation.responseDigest = responseDigest
	operation.handling = true
	return operation, false, nil
}

func (r *renameTracker) commitResponse(operationID string, operation *renameOutcome) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[operationID] != operation || !operation.handling {
		return &ProtocolError{Category: CategoryProtocol, Detail: "rename response lost its outstanding durable correlation"}
	}
	delete(r.pending, operationID)
	operation.handling = false
	close(operation.done)
	r.rememberLocked(operationID, operation)
	return nil
}

func (r *renameTracker) failHandler(operationID string, operation *renameOutcome, handlerErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[operationID] != operation {
		return
	}
	delete(r.pending, operationID)
	operation.handling = false
	operation.result = SessionRename{}
	operation.err = renameError(renameFailureFromError(handlerErr), handlerErr.Error())
	close(operation.done)
	if operation.err.Category == RenameProtocol || operation.err.Category == RenameUnsupported || operation.err.Category == RenameNativeRejected {
		r.rememberLocked(operationID, operation)
	}
}

func (r *renameTracker) reject(frame Frame) (bool, error) {
	var rejection Reject
	if err := frame.PayloadInto(&rejection); err != nil {
		return false, err
	}
	if rejection.OperationID != frame.ID {
		return false, &ProtocolError{Category: CategoryProtocol, Detail: "rename rejection operation id does not match frame id"}
	}
	digest := sha256.Sum256(frame.Payload)
	r.mu.Lock()
	defer r.mu.Unlock()
	if completed := r.completed[frame.ID]; completed != nil {
		if completed.responseDigest == digest && completed.err != nil {
			return true, nil
		}
		return false, &ProtocolError{Category: CategoryReplay, Detail: "rename rejection conflicts with completed native evidence"}
	}
	operation := r.pending[frame.ID]
	if operation == nil {
		return false, &ProtocolError{Category: CategoryProtocol, Detail: "rename rejection does not name an outstanding daemon request"}
	}
	delete(r.pending, frame.ID)
	operation.err = renameError(renameFailureFromProtocol(rejection.Category), Redact(rejection.Detail))
	operation.responseDigest = digest
	close(operation.done)
	r.rememberLocked(frame.ID, operation)
	return false, nil
}

func (r *renameTracker) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for operationID, operation := range r.pending {
		delete(r.pending, operationID)
		operation.err = renameError(RenameUnavailable, "component binding disconnected before native rename acceptance")
		close(operation.done)
		r.rememberLocked(operationID, operation)
	}
}

func (r *renameTracker) timeOut(operationID string, operation *renameOutcome) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[operationID] != operation || operation.handling {
		return false
	}
	delete(r.pending, operationID)
	operation.err = renameError(RenameTimedOut, "component native rename did not complete before the request deadline")
	close(operation.done)
	r.rememberLocked(operationID, operation)
	return true
}

func (r *renameTracker) rememberLocked(operationID string, operation *renameOutcome) {
	r.completed[operationID] = operation
	r.completedOrder = append(r.completedOrder, operationID)
	for uint64(len(r.completedOrder)) > r.replayWindow {
		oldest := r.completedOrder[0]
		r.completedOrder = r.completedOrder[1:]
		delete(r.completed, oldest)
	}
}

func (b *binding) requestRename(ctx context.Context, operationID string, request SessionRenameRequest) (SessionRename, error) {
	operation, created, err := b.renames.reserve(operationID, request)
	if err != nil {
		return SessionRename{}, err
	}
	if created {
		if err := b.send(TypeSessionRenameRequest, operationID, request); err != nil {
			b.renames.failSend(operationID, operation, err)
		}
	}
	select {
	case <-operation.done:
		if operation.err != nil {
			return SessionRename{}, operation.err
		}
		return operation.result, nil
	case <-ctx.Done():
		if created && b.renames.timeOut(operationID, operation) {
			return SessionRename{}, operation.err
		}
		return SessionRename{}, renameError(RenameTimedOut, "component native rename did not complete before the request deadline")
	}
}

// RenameSession writes a native title through the exact live component
// binding and waits for its correlated native acceptance. operationID must be
// stable and use DaemonRenameOperationPrefix.
func (b *Broker) RenameSession(ctx context.Context, bindingID, operationID string, request SessionRenameRequest) (SessionRename, error) {
	if !validDaemonRenameOperationID(operationID) {
		return SessionRename{}, renameError(RenameProtocol, "daemon rename operation id uses the wrong namespace")
	}
	frame, err := NewFrame(TypeSessionRenameRequest, operationID, 1, request)
	if err != nil {
		return SessionRename{}, renameError(RenameProtocol, err.Error())
	}
	if err := ValidatePayload(frame); err != nil {
		return SessionRename{}, renameError(RenameProtocol, err.Error())
	}
	b.mu.Lock()
	live := b.bindings[bindingID]
	b.mu.Unlock()
	if live == nil {
		return SessionRename{}, renameError(RenameUnavailable, "component binding is unavailable")
	}
	return live.requestRename(ctx, operationID, request)
}

func renameError(category RenameFailureCategory, detail string) *RenameError {
	return &RenameError{Category: category, Detail: Redact(detail)}
}

func renameFailureFromProtocol(category Category) RenameFailureCategory {
	switch category {
	case CategoryUnknownType, Category("unsupported"):
		return RenameUnsupported
	case Category("unavailable"), Category("inactive"):
		return RenameUnavailable
	case Category("timed-out"):
		return RenameTimedOut
	case Category("native-rejected"):
		return RenameNativeRejected
	case CategoryInternal:
		return RenameUnavailable
	default:
		return RenameProtocol
	}
}

func renameFailureFromError(err error) RenameFailureCategory {
	var protocolErr *ProtocolError
	if errors.As(err, &protocolErr) {
		return renameFailureFromProtocol(protocolErr.Category)
	}
	return RenameUnavailable
}

func renameRequestDigest(request SessionRenameRequest) [sha256.Size]byte {
	return sha256.Sum256([]byte(request.NativeSessionID + "\x00" + request.RequestedName))
}

var _ error = (*RenameError)(nil)
