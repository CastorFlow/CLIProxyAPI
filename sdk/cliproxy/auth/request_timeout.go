package auth

import (
	"context"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// requestTimeoutsFor resolves the effective timeouts for an attempt. The first
// matching rule for the requested model, protocols, or provider wins.
func requestTimeoutsFor(cfg *internalconfig.Config, model string, protocols, providers []string) internalconfig.ResolvedRequestTimeouts {
	if cfg == nil {
		return (&internalconfig.RequestTimeoutConfig{}).Resolve(model, protocols, providers)
	}
	return cfg.Timeouts.Resolve(model, protocols, providers)
}

// requestTimeoutsForAttempt resolves the timeouts for one execution attempt.
// The requested model (not the credential-specific upstream model) is matched so
// a rule keyed to "claude-*" also covers the concrete model the alias resolves to.
func (m *Manager) requestTimeoutsForAttempt(routeModel string, opts executor.Options, provider string) internalconfig.ResolvedRequestTimeouts {
	protocols := make([]string, 0, 2)
	if source := opts.SourceFormat.String(); source != "" {
		protocols = append(protocols, source)
	}
	providers := make([]string, 0, 1)
	if provider = strings.TrimSpace(provider); provider != "" {
		providers = append(providers, provider)
	}
	return requestTimeoutsFor(m.runtimeConfigSnapshot(), routeModel, protocols, providers)
}

func requestTimeoutMessage(kind string, d time.Duration) string {
	return "upstream " + kind + " timeout after " + d.String()
}

// executeWithRequestTimeout runs one non-streaming upstream attempt under the
// configured total bound. The timeout fires on the wall clock; it does not
// depend on the executor honoring context cancellation.
func executeWithRequestTimeout(ctx context.Context, providerExecutor ProviderExecutor, auth *Auth, req executor.Request, opts executor.Options, bound time.Duration) (executor.Response, error) {
	if bound <= 0 {
		return providerExecutor.Execute(ctx, auth, req, opts)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cancelErr := NewRequestTimeoutError(nil, requestTimeoutMessage("non-stream", bound))
	opCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type execResult struct {
		resp executor.Response
		err  error
	}
	done := make(chan execResult, 1)
	go func() {
		resp, errExec := providerExecutor.Execute(opCtx, auth, req, opts)
		done <- execResult{resp: resp, err: errExec}
	}()

	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case out := <-done:
		if errOut := out.err; errOut != nil && context.Cause(opCtx) == cancelErr {
			return executor.Response{}, cancelErr
		}
		return out.resp, out.err
	case <-opCtx.Done():
		if cause := context.Cause(opCtx); cause == cancelErr {
			return executor.Response{}, cancelErr
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return executor.Response{}, errCtx
		}
		return executor.Response{}, context.Canceled
	case <-timer.C:
		cancel(cancelErr)
		return executor.Response{}, cancelErr
	}
}

// streamTimeoutTracker enforces the configured streaming bounds over a provider
// stream. It is owned by the single reader of the executor's chunk channel, so
// no extra channel, goroutine, or hop is introduced on the streaming hot path.
type streamTimeoutTracker struct {
	firstByte time.Duration
	idle      time.Duration

	lastActivity  time.Time
	firstByteSeen bool
}

// newStreamTimeoutTracker builds a tracker for one execution attempt. When the
// first-byte bound is disabled but the idle bound is enabled, idle also applies
// before the first payload so a stalled stream is still caught.
func newStreamTimeoutTracker(resolved internalconfig.ResolvedRequestTimeouts) *streamTimeoutTracker {
	firstByte := resolved.StreamFirstByte
	if firstByte <= 0 {
		firstByte = resolved.StreamIdle
	}
	return &streamTimeoutTracker{
		firstByte:    firstByte,
		idle:         resolved.StreamIdle,
		lastActivity: time.Now(),
	}
}

// disabled reports whether no bound applies to this stream.
func (t *streamTimeoutTracker) disabled() bool {
	return t == nil || (t.firstByte <= 0 && t.idle <= 0)
}

// observeData records a payload chunk delivered by the provider. Only a data
// chunk counts as activity: upstream keep-alive and control frames the client
// never sees must not extend the wait for real output.
func (t *streamTimeoutTracker) observeData(chunk executor.StreamChunk) {
	if t == nil || len(chunk.Payload) == 0 {
		return
	}
	t.lastActivity = time.Now()
	t.firstByteSeen = true
}

func (t *streamTimeoutTracker) bound() (time.Time, string, time.Duration) {
	if !t.firstByteSeen && t.firstByte > 0 {
		return t.lastActivity.Add(t.firstByte), "stream first-byte", t.firstByte
	}
	if t.idle > 0 {
		return t.lastActivity.Add(t.idle), "stream idle", t.idle
	}
	return time.Time{}, "", 0
}

// read returns the next chunk from ch, or a request timeout error once the
// applicable bound is exceeded. Deadlines are absolute, so time the caller
// spends outside this call still counts as silence.
//
// A chunk that is already available is preferred over a context cancellation:
// an upstream error delivered together with a cancellation must still reach the
// caller so its cooldown is recorded (upstream 5dbd92a0, "preserve upstream
// error cooldown on canceled stream contexts").
func (t *streamTimeoutTracker) read(ctx context.Context, ch <-chan executor.StreamChunk) (executor.StreamChunk, bool, error) {
	if t.disabled() {
		if ctx == nil {
			chunk, ok := <-ch
			return chunk, ok, nil
		}
		select {
		case chunk, ok := <-ch:
			return chunk, ok, nil
		default:
			select {
			case <-ctx.Done():
				return executor.StreamChunk{}, false, ctx.Err()
			case chunk, ok := <-ch:
				return chunk, ok, nil
			}
		}
	}
	deadline, kind, bound := t.bound()
	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			return executor.StreamChunk{}, false, NewRequestTimeoutError(nil, requestTimeoutMessage(kind, bound))
		}
		timer := time.NewTimer(wait)
		var (
			chunk     executor.StreamChunk
			ok        bool
			delivered bool
		)
		if ctx == nil {
			select {
			case chunk, ok = <-ch:
				delivered = true
			case <-timer.C:
			}
		} else {
			select {
			case chunk, ok = <-ch:
				delivered = true
			default:
				select {
				case <-ctx.Done():
					timer.Stop()
					return executor.StreamChunk{}, false, ctx.Err()
				case chunk, ok = <-ch:
					delivered = true
				case <-timer.C:
				}
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if delivered {
			// Includes the closed-channel case, where ok is false and the caller
			// must see the end of the stream rather than another timeout.
			return chunk, ok, nil
		}
		if errCtx := contextErr(ctx); errCtx != nil {
			return executor.StreamChunk{}, false, errCtx
		}
	}
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
