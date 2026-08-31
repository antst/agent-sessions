package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	daemonpkg "github.com/antst/agent-sessions/internal/daemon"
)

func pendingAttachmentEvidence[T any](
	lock sync.Locker,
	pending map[string]*T,
	attachmentID, product string,
	project func(*T) daemonpkg.NativeEvidence,
) (daemonpkg.NativeEvidence, error) {
	lock.Lock()
	defer lock.Unlock()
	record := pending[attachmentID]
	if record == nil {
		return daemonpkg.NativeEvidence{}, fmt.Errorf("%s native preparation is unavailable", product)
	}
	return project(record), nil
}

func pendingAttachmentEvidenceChecked[T any](
	lock sync.Locker,
	pending map[string]*T,
	attachmentID, product string,
	project func(*T) (daemonpkg.NativeEvidence, error),
) (daemonpkg.NativeEvidence, error) {
	lock.Lock()
	record := pending[attachmentID]
	lock.Unlock()
	if record == nil {
		return daemonpkg.NativeEvidence{}, fmt.Errorf("%s native preparation is unavailable", product)
	}
	return project(record)
}

func requestPreparation[Request any, Result any](
	ctx context.Context,
	operation string,
	request Request,
) (Result, error) {
	var result Result
	payload, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	id := commandRequestID()
	response, err := callExistingDaemon(ctx, defaultStateRoot(), daemonpkg.ControlRequest{
		ID: id, Role: daemonpkg.RoleLauncher, Operation: operation,
		IdempotencyKey: id, Payload: payload,
	})
	if err != nil {
		return result, err
	}
	if response.Error != nil {
		return result, errors.New(response.Error.Message)
	}
	if json.Unmarshal(response.Payload, &result) != nil {
		return result, errors.New("daemon returned an invalid product handoff")
	}
	return result, nil
}
