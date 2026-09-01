package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNativeLaneACPExecutionTimeoutStartsAfterSetup(t *testing.T) {
	timeout := 20 * time.Millisecond
	time.Sleep(2 * timeout)
	started := time.Now()
	err := runNativeLaneACPExecution(context.Background(), timeout, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("execution context inherited an already-expired setup deadline: %v", err)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("execution timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < timeout/2 {
		t.Fatalf("execution timeout elapsed in %v, want a fresh %v window", elapsed, timeout)
	}
}
