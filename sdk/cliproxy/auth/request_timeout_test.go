package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type requestTimeoutTestExecutor struct {
	block         bool
	payload       []byte
	returnCtxErr  bool
	releaseOnCtx  bool
	cancelObserve chan struct{}
}

func (e *requestTimeoutTestExecutor) Identifier() string { return "codex" }

func (e *requestTimeoutTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestTimeoutTestExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if !e.block {
		return cliproxyexecutor.Response{Payload: e.payload}, nil
	}
	select {
	case <-ctx.Done():
		if e.cancelObserve != nil {
			close(e.cancelObserve)
		}
		if e.returnCtxErr {
			return cliproxyexecutor.Response{}, ctx.Err()
		}
		// A misbehaving executor that ignores cancellation would otherwise hang
		// the caller forever; the wall-clock timer must still bound the request.
		time.Sleep(2 * time.Second)
		return cliproxyexecutor.Response{Payload: e.payload}, nil
	case <-time.After(5 * time.Second):
		return cliproxyexecutor.Response{Payload: e.payload}, nil
	}
}

func (e *requestTimeoutTestExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	panic("not implemented")
}

func (e *requestTimeoutTestExecutor) CountTokens(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	panic("not implemented")
}

func (e *requestTimeoutTestExecutor) HttpRequest(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	panic("not implemented")
}

func TestExecuteWithRequestTimeoutDisabled(t *testing.T) {
	exec := &requestTimeoutTestExecutor{payload: []byte("ok")}
	resp, err := executeWithRequestTimeout(context.Background(), exec, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, 0)
	if err != nil || string(resp.Payload) != "ok" {
		t.Fatalf("resp=%q err=%v, want passthrough", resp.Payload, err)
	}
}

func TestExecuteWithRequestTimeoutFastSuccess(t *testing.T) {
	exec := &requestTimeoutTestExecutor{payload: []byte("ok")}
	resp, err := executeWithRequestTimeout(context.Background(), exec, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, time.Second)
	if err != nil || string(resp.Payload) != "ok" {
		t.Fatalf("resp=%q err=%v, want passthrough", resp.Payload, err)
	}
}

func TestExecuteWithRequestTimeoutFiresAndClassifies(t *testing.T) {
	observed := make(chan struct{})
	exec := &requestTimeoutTestExecutor{block: true, cancelObserve: observed}
	start := time.Now()
	_, err := executeWithRequestTimeout(context.Background(), exec, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, 20*time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatalf("timeout did not bound the call")
	}
	if !IsRequestTimeoutError(err) {
		t.Fatalf("err = %v, want request timeout error", err)
	}
	var sc interface{ StatusCode() int }
	if !errors.As(err, &sc) || sc.StatusCode() != http.StatusGatewayTimeout {
		t.Fatalf("status = %v, want 504", err)
	}
	if !isRequestScopedError(err) {
		t.Fatal("timeout must be request-scoped so the credential is not cooled down")
	}
	if !strings.Contains(err.Error(), "non-stream") {
		t.Fatalf("message = %q, want non-stream marker", err.Error())
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("executor context was not canceled after the timeout")
	}
}

func TestExecuteWithRequestTimeoutIgnoresExecutorCancellationError(t *testing.T) {
	exec := &requestTimeoutTestExecutor{block: true, returnCtxErr: true}
	_, err := executeWithRequestTimeout(context.Background(), exec, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, 20*time.Millisecond)
	if !IsRequestTimeoutError(err) {
		t.Fatalf("err = %v, want request timeout error instead of context.DeadlineExceeded", err)
	}
}

func TestExecuteWithRequestTimeoutRespectsCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec := &requestTimeoutTestExecutor{block: true}
	_, err := executeWithRequestTimeout(ctx, exec, nil, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRequestTimeoutResultErrorIsRequestScoped(t *testing.T) {
	err := NewRequestTimeoutError(nil, "upstream stream idle timeout after 5m0s")
	resultErr := resultErrorFromError(err)
	if resultErr == nil || resultErr.StatusCode() != http.StatusGatewayTimeout {
		t.Fatalf("resultErr = %+v, want 504", resultErr)
	}
	if resultErr.Code != ErrorCodeRequestScoped {
		t.Fatalf("code = %q, want %q", resultErr.Code, ErrorCodeRequestScoped)
	}
	if !shouldSkipCredentialCooldown(resultErr) {
		t.Fatal("timeout must not cool down the credential")
	}
	// A typed status keeps this on the request-scoped path rather than the
	// connection-lifecycle fallback, which must never cool down a credential.
	if isConnectionLifecycleError(err) {
		t.Fatal("timeout with a 504 status must not fall back to connection-lifecycle classification")
	}
}

func TestStreamTimeoutTrackerFirstByteTimeout(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	tracker := &streamTimeoutTracker{firstByte: 10 * time.Millisecond, idle: time.Hour, lastActivity: time.Now()}
	_, _, err := tracker.read(context.Background(), ch)
	if !IsRequestTimeoutError(err) || !strings.Contains(err.Error(), "first-byte") {
		t.Fatalf("err = %v, want first-byte timeout", err)
	}
}

func TestStreamTimeoutTrackerIdleTimeout(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	tracker := &streamTimeoutTracker{firstByte: time.Hour, idle: 10 * time.Millisecond, lastActivity: time.Now(), firstByteSeen: true}
	_, _, err := tracker.read(context.Background(), ch)
	if !IsRequestTimeoutError(err) || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("err = %v, want idle timeout", err)
	}
}

func TestStreamTimeoutTrackerDisabled(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte("x")}
	tracker := newStreamTimeoutTracker(internalconfig.ResolvedRequestTimeouts{})
	if !tracker.disabled() {
		t.Fatal("tracker with no bounds must be disabled")
	}
	chunk, ok, err := tracker.read(context.Background(), ch)
	if err != nil || !ok || string(chunk.Payload) != "x" {
		t.Fatalf("chunk=%q ok=%v err=%v", chunk.Payload, ok, err)
	}
}

func TestStreamTimeoutTrackerClosedChannelEndsStream(t *testing.T) {
	// Regression: a closed channel must end the stream instead of being retried
	// until a bound fires.
	ch := make(chan cliproxyexecutor.StreamChunk)
	close(ch)
	tracker := &streamTimeoutTracker{firstByte: time.Hour, idle: time.Hour, lastActivity: time.Now()}
	_, ok, err := tracker.read(context.Background(), ch)
	if err != nil {
		t.Fatalf("err = %v, want a clean end of stream", err)
	}
	if ok {
		t.Fatal("closed channel must not report a chunk")
	}
}

func TestStreamTimeoutTrackerUsesAbsoluteDeadline(t *testing.T) {
	// A stalled read must not have its budget restarted by when it starts.
	ch := make(chan cliproxyexecutor.StreamChunk)
	tracker := &streamTimeoutTracker{firstByte: 10 * time.Millisecond, idle: time.Hour, lastActivity: time.Now().Add(-time.Second)}
	_, _, err := tracker.read(context.Background(), ch)
	if !IsRequestTimeoutError(err) {
		t.Fatalf("err = %v, want an immediate timeout for a stale activity time", err)
	}
}

func TestStreamTimeoutTrackerCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan cliproxyexecutor.StreamChunk)
	tracker := &streamTimeoutTracker{firstByte: time.Hour, idle: time.Hour, lastActivity: time.Now()}
	_, _, err := tracker.read(ctx, ch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStreamTimeoutTrackerObserveIgnoresControlFrames(t *testing.T) {
	// Keep-alive and control frames reach this tracker but never the client, so
	// they must neither end the first-byte phase nor extend the silence clock.
	tracker := &streamTimeoutTracker{firstByte: time.Second, idle: 10 * time.Second, lastActivity: time.Now()}
	early := tracker.lastActivity
	time.Sleep(2 * time.Millisecond)
	tracker.observeData(cliproxyexecutor.StreamChunk{Payload: nil})
	if tracker.firstByteSeen {
		t.Fatal("an empty control frame must not end the first-byte phase")
	}
	if !tracker.lastActivity.Equal(early) {
		t.Fatal("an empty control frame must not reset the silence clock")
	}
	tracker.observeData(cliproxyexecutor.StreamChunk{Payload: []byte("x")})
	if !tracker.firstByteSeen {
		t.Fatal("a payload chunk must end the first-byte phase")
	}
	if !tracker.lastActivity.After(early) {
		t.Fatal("a payload chunk must reset the silence clock")
	}
}

func TestStreamTimeoutTrackerIdleFallbackForFirstBytePhase(t *testing.T) {
	// A disabled first-byte bound still leaves the idle bound guarding a stream
	// that never produces its first payload.
	tracker := newStreamTimeoutTracker(internalconfig.ResolvedRequestTimeouts{StreamIdle: time.Millisecond})
	deadline, kind, bound := tracker.bound()
	if deadline.IsZero() || !strings.Contains(kind, "first-byte") || bound != time.Millisecond {
		t.Fatalf("bound() = (%v, %q, %v), want the idle bound to cover the first byte", deadline, kind, bound)
	}
}

func TestRequestTimeoutsForDefaults(t *testing.T) {
	got := requestTimeoutsFor(nil, "gpt-5", nil, nil)
	if got.StreamFirstByte != 60*time.Second || got.StreamIdle != 300*time.Second || got.NonStream != 600*time.Second {
		t.Fatalf("timeouts = %+v, want documented defaults", got)
	}
}
