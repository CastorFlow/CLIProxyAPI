package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// stallingRequestTimeoutExecutor never completes on its own so the configured
// request timeouts are the only thing that can end an attempt.
type stallingRequestTimeoutExecutor struct {
	firstPayload []byte
	closeStream  bool
}

func (e *stallingRequestTimeoutExecutor) Identifier() string { return "claude" }

func (e *stallingRequestTimeoutExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *stallingRequestTimeoutExecutor) Execute(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	<-ctx.Done()
	// Deliberately ignore cancellation so the request is bounded by wall clock.
	time.Sleep(3 * time.Second)
	return cliproxyexecutor.Response{}, ctx.Err()
}

func (e *stallingRequestTimeoutExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if len(e.firstPayload) > 0 {
		chunks <- cliproxyexecutor.StreamChunk{Payload: e.firstPayload}
	}
	if e.closeStream {
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	// Left open: the stream stalls and only a configured timeout can end it.
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *stallingRequestTimeoutExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	panic("not implemented")
}

func (e *stallingRequestTimeoutExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	panic("not implemented")
}

func newRequestTimeoutTestManager(t *testing.T, executor ProviderExecutor, cfg *internalconfig.Config) (*Manager, *Auth, string) {
	t.Helper()
	model := "request-timeout-model-" + uuid.NewString()
	auth := &Auth{
		ID:         "request-timeout-auth-" + uuid.NewString(),
		Provider:   "claude",
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"request_retry": float64(0),
		},
	}
	manager := NewManager(nil, nil, NoopHook{})
	manager.SetRetryConfig(0, 0, 0)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(executor)
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return manager, auth, model
}

func timeoutTestConfig(nonStreamSeconds, firstByteSeconds, idleSeconds int) *internalconfig.Config {
	cfg := &internalconfig.Config{}
	rule := internalconfig.RequestTimeoutRule{Providers: []string{"claude"}}
	if nonStreamSeconds > 0 {
		v := nonStreamSeconds
		rule.NonStreamSeconds = &v
	}
	if firstByteSeconds > 0 {
		v := firstByteSeconds
		rule.StreamFirstByteSeconds = &v
	}
	if idleSeconds > 0 {
		v := idleSeconds
		rule.StreamIdleSeconds = &v
	}
	cfg.Timeouts.Rules = []internalconfig.RequestTimeoutRule{rule}
	return cfg
}

func TestManagerNonStreamRequestTimeoutFiresAndStaysNeutral(t *testing.T) {
	executor := &stallingRequestTimeoutExecutor{}
	manager, auth, model := newRequestTimeoutTestManager(t, executor, timeoutTestConfig(1, 0, 0))

	start := time.Now()
	_, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if !IsRequestTimeoutError(errExecute) {
		t.Fatalf("Execute() error = %v, want request timeout error", errExecute)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Execute() took %s, want the configured bound to end the attempt", elapsed)
	}

	// The credential answered; only the request exceeded a bound, so it must stay
	// eligible and uncooled.
	requireClaudeCancellationNeutral(t, manager, auth.ID, model)
}

func TestManagerStreamFirstByteTimeoutFailsBeforeHeaders(t *testing.T) {
	executor := &stallingRequestTimeoutExecutor{}
	manager, auth, model := newRequestTimeoutTestManager(t, executor, timeoutTestConfig(0, 1, 0))

	start := time.Now()
	_, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if !IsRequestTimeoutError(errStream) {
		t.Fatalf("ExecuteStream() error = %v, want request timeout error", errStream)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ExecuteStream() took %s, want the configured bound to end the attempt", elapsed)
	}
	requireClaudeCancellationNeutral(t, manager, auth.ID, model)
}

func TestManagerStreamIdleTimeoutSurfacesMidStream(t *testing.T) {
	executor := &stallingRequestTimeoutExecutor{firstPayload: []byte("first")}
	manager, _, model := newRequestTimeoutTestManager(t, executor, timeoutTestConfig(0, 1, 1))

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	first, ok := <-stream.Chunks
	if !ok || string(first.Payload) != "first" {
		t.Fatalf("first chunk = %#v ok=%v, want the upstream payload", first, ok)
	}

	select {
	case timeout, ok := <-stream.Chunks:
		if !ok {
			t.Fatal("stream closed without reporting the idle timeout")
		}
		if !IsRequestTimeoutError(timeout.Err) {
			t.Fatalf("chunk error = %v, want request timeout error", timeout.Err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("idle timeout was not enforced mid-stream")
	}
}

func TestManagerDisabledStreamTimeoutStreamsNormally(t *testing.T) {
	executor := &stallingRequestTimeoutExecutor{firstPayload: []byte("first"), closeStream: true}
	manager, _, model := newRequestTimeoutTestManager(t, executor, timeoutTestConfig(0, 1, 0))

	stream, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	first, ok := <-stream.Chunks
	if !ok || string(first.Payload) != "first" {
		t.Fatalf("first chunk = %#v ok=%v", first, ok)
	}
	if _, ok := <-stream.Chunks; ok {
		t.Fatal("stream should end after the upstream closes")
	}
}
