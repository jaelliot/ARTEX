package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Explicit opt-in only: the parent fixture supplies a root-owned broker socket
// read-only to a disconnected test container, without a Docker socket.
func TestDeployedBroker(t *testing.T) {
	path := os.Getenv("ARTEX_TEST_WORKER_SOCKET")
	if path == "" {
		t.Skip("requires the dedicated deployed-worker fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := Run(ctx, path, `test "$(id -u)" = 65534 && test ! -S /var/run/docker.sock && echo fixture > /work/marker && printf deployed-worker`)
	if err != nil || !result.OK || result.Output != "deployed-worker" {
		t.Fatalf("real worker failed: %+v %v", result, err)
	}
	result, err = Run(ctx, path, "test ! -e /work/marker && printf fresh-workspace")
	if err != nil || !result.OK || result.Output != "fresh-workspace" {
		t.Fatalf("worker workspace persisted: %+v %v", result, err)
	}
	jobCtx, stopJob := context.WithTimeout(ctx, 2*time.Second)
	defer stopJob()
	_, err = Run(jobCtx, path, "sleep 120")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("real worker cancellation: %v", err)
	}
	result, err = Run(ctx, path, "printf after-cancellation")
	if err != nil || !result.OK || result.Output != "after-cancellation" {
		t.Fatalf("broker did not recover after cancellation: %+v %v", result, err)
	}
}
