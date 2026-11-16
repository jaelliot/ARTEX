// Package worker communicates with the isolated offline worker broker.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const SocketEnv = "ARTEX_OFFLINE_WORKER_SOCKET"

// SocketPath reports whether offline worker mode was explicitly configured.
// A configured empty path is still enabled and fails closed when used.
func SocketPath() (string, bool) { return os.LookupEnv(SocketEnv) }

const (
	maxRequest  = 65536
	maxResponse = 12*1024*1024 + 4096
	maxDuration = 200 * time.Second
)

type Result struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
}

// Run sends one script to the broker. Cancellation closes the connection so
// the broker can terminate the worker rather than leaving it running.
func Run(ctx context.Context, socketPath, script string) (Result, error) {
	if !filepath.IsAbs(socketPath) {
		return Result{}, errors.New("worker socket path must be absolute")
	}
	request, err := json.Marshal(struct {
		Script string `json:"script"`
	}{Script: script})
	if err != nil {
		return Result{}, err
	}
	request = append(request, '\n')
	if len(request) > maxRequest {
		return Result{}, errors.New("worker request is too large")
	}

	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Result{}, fmt.Errorf("connect to worker broker: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return Result{}, err
		}
	}
	if _, err := conn.Write(request); err != nil {
		return Result{}, fmt.Errorf("send worker request: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxResponse+1)).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("read worker response: %w", err)
	}
	if len(line) > maxResponse {
		return Result{}, errors.New("worker response is too large")
	}
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("invalid worker response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("worker response has trailing data")
	}
	if result.OK && result.Error != "" {
		return Result{}, errors.New("successful worker response contains an error")
	}
	return result, nil
}
