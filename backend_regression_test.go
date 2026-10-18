package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the findings of the lobaro-backend#1625 review.
// A backend main() needs to derive its exit code and its shutdown handling from the Container API only.

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// mustStop waits until all services stopped and fails on timeout, unlike waitStopped.
func mustStop(t *testing.T, c *service.Container) {
	t.Helper()
	waitStopped(c, 2*time.Second)
	require.Equal(t, 0, c.RunningCount(), "services still running after timeout: %v", c.RunningServiceNames())
}

// requireStopReason fails unless StopCause() is a *StopError with the given reason.
func requireStopReason(t *testing.T, c *service.Container, reason service.StopReason) *service.StopError {
	t.Helper()
	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	require.Equal(t, reason, stopErr.Reason)
	return stopErr
}

// isUnexpectedStop is what a backend main() uses to derive a non-zero exit code from StopCause().
// A cause that is not a *StopError is treated as unexpected.
func isUnexpectedStop(cause error) bool {
	var stopErr *service.StopError
	if !errors.As(cause, &stopErr) {
		return cause != nil
	}
	switch stopErr.Reason {
	case service.StopReasonStopAll, service.StopReasonParentContext:
		return false
	}
	return true
}

// Finding 1: panic in Run/Init is recognizable via errors.As and carries the service name.
func TestPanicStopCause_RunAndInit(t *testing.T) {
	tests := map[string]struct {
		register func(c *service.Container)
	}{
		"run": {func(c *service.Container) {
			service.New("parser").Run(func(ctx context.Context) error { panic("boom") }).Register(c)
		}},
		"init": {func(c *service.Container) {
			service.New("parser").Init(func(ctx context.Context) error { panic("boom") }).Run(waitCtx).Register(c)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newLoggedContainer(t)
			service.New("healthy").Run(waitCtx).Register(c)
			tt.register(c)
			_ = c.StartAll(context.Background())
			mustStop(t, c)

			cause := c.StopCause()
			require.Error(t, cause)
			var stopErr *service.StopError
			require.ErrorAs(t, cause, &stopErr)
			assert.Equal(t, service.StopReasonPanic, stopErr.Reason)
			assert.Equal(t, "parser", stopErr.Service)
			assert.Contains(t, cause.Error(), "parser")
			var panicErr *service.PanicError
			require.ErrorAs(t, cause, &panicErr)
			assert.Equal(t, "boom", panicErr.Value)
			assert.NotEmpty(t, panicErr.Stack)
			assert.True(t, isUnexpectedStop(cause))
		})
	}
}

// Finding 1: panic, parent cancel and StopAll are distinguishable, only the panic means exit code != 0.
func TestStopCause_DistinguishPanicFromParentCancelAndStopAll(t *testing.T) {
	tests := []struct {
		name       string
		reason     service.StopReason
		trigger    func(c *service.Container, cancel context.CancelFunc)
		run        service.RunFunc
		unexpected bool
		isPanic    bool
	}{
		{"parent cancel", service.StopReasonParentContext, func(c *service.Container, cancel context.CancelFunc) { cancel() }, returnCtxErr, false, false},
		{"StopAll", service.StopReasonStopAll, func(c *service.Container, cancel context.CancelFunc) { c.StopAll() }, returnCtxErr, false, false},
		{"panic", service.StopReasonPanic, func(c *service.Container, cancel context.CancelFunc) {}, func(ctx context.Context) error { panic("boom") }, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newLoggedContainer(t)
			service.New("db").Run(tt.run).Register(c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			require.NoError(t, c.StartAll(ctx))
			tt.trigger(c, cancel)
			mustStop(t, c)

			requireStopReason(t, c, tt.reason)
			assert.Equal(t, tt.unexpected, isUnexpectedStop(c.StopCause()))
			var pe *service.PanicError
			assert.Equal(t, tt.isPanic, errors.As(c.StopCause(), &pe))
		})
	}
}

// Finding 1: a Run error is an unexpected stop, a nil return before the stop is no stop at all.
func TestStopCause_RunErrorIsUnexpected(t *testing.T) {
	c, _ := newLoggedContainer(t)
	runErr := errors.New("run failed")
	service.New("worker").Run(func(ctx context.Context) error { return runErr }).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	mustStop(t, c)
	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonServiceFailed, stopErr.Reason)
	assert.ErrorIs(t, c.StopCause(), runErr)
	assert.True(t, isUnexpectedStop(c.StopCause()))
}

func TestStopCause_NilBeforeStopIsNoStop(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("worker").Run(func(ctx context.Context) error { return nil }).Register(c)
	service.New("http").Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	require.Eventually(t, func() bool { return c.RunningCount() == 1 }, time.Second, time.Millisecond)
	assert.NoError(t, c.StopCause())
	c.StopAll()
	mustStop(t, c)
}

// Finding 2: OnShutdown registered before StartAll fires on a panic/error during Init.
func TestOnShutdownBeforeStartAll_FiresOnInitFailure(t *testing.T) {
	tests := map[string]func(ctx context.Context) error{
		"panic": func(ctx context.Context) error { panic("boom") },
		"error": func(ctx context.Context) error { return errors.New("init failed") },
	}
	for name, initFn := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newLoggedContainer(t)
			var calls atomic.Int32
			c.OnShutdown(func() { calls.Add(1) })
			service.New("db").Init(initFn).Run(waitCtx).Register(c)

			require.Error(t, c.StartAll(context.Background()))
			// The callback runs synchronously in StartAll when Init fails
			assert.Equal(t, int32(1), calls.Load())
			mustStop(t, c)
			assert.Equal(t, int32(1), calls.Load(), "called exactly once")
		})
	}
}

// Finding 2: OnShutdown registered before StartAll fires on a panic/error during Run (start phase).
func TestOnShutdownBeforeStartAll_FiresOnRunFailure(t *testing.T) {
	tests := map[string]func(ctx context.Context) error{
		"panic": func(ctx context.Context) error { panic("boom") },
		"error": func(ctx context.Context) error { return errors.New("run failed") },
	}
	for name, runFn := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := newLoggedContainer(t)
			called := make(chan struct{}, 2)
			c.OnShutdown(func() { called <- struct{}{} })
			service.New("worker").Run(runFn).Register(c)
			service.New("http").Run(waitCtx).Register(c)

			_ = c.StartAll(context.Background())
			waitSignal(t, called, "OnShutdown")
			mustStop(t, c)
			select {
			case <-called:
				t.Fatal("OnShutdown called twice")
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

// Finding 2: OnShutdown registered after the container stopped is called immediately (pinned behavior, see OnShutdown doc).
func TestOnShutdownAfterStop_FiresImmediately(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	mustStop(t, c)

	var calls atomic.Int32
	c.OnShutdown(func() { calls.Add(1) })
	assert.Equal(t, int32(1), calls.Load(), "called synchronously in OnShutdown")
}

// Finding 2: OnShutdown registered after an init failure is called immediately as well.
func TestOnShutdownAfterInitFailure_FiresImmediately(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Init(func(ctx context.Context) error { return errors.New("nope") }).Register(c)
	require.Error(t, c.StartAll(context.Background()))

	var calls atomic.Int32
	c.OnShutdown(func() { calls.Add(1) })
	assert.Equal(t, int32(1), calls.Load())
}

// Finding 2: StopAll is recognizable as intended stop via StopCause, also while it still runs in StartupMode (during Init).
func TestStopAllDuringStart_IsIntendedStop(t *testing.T) {
	c, _ := newLoggedContainer(t)
	initStarted := make(chan struct{})
	release := make(chan struct{})
	service.New("slow").Init(func(ctx context.Context) error {
		close(initStarted)
		<-release
		return nil
	}).Run(waitCtx).Register(c)
	service.New("http").Run(waitCtx).Register(c)

	startErr := make(chan error, 1)
	go func() { startErr <- c.StartAll(context.Background()) }()
	waitSignal(t, initStarted, "Init")
	c.StopAll()
	close(release)

	select {
	case err := <-startErr:
		require.Error(t, err)
		var stopErr *service.StopError
		require.ErrorAs(t, err, &stopErr)
		assert.Equal(t, service.StopReasonStopAll, stopErr.Reason)
	case <-time.After(3 * time.Second):
		t.Fatal("StartAll did not return")
	}
	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonStopAll, stopErr.Reason)
	assert.False(t, isUnexpectedStop(c.StopCause()))
	mustStop(t, c)
	assert.Equal(t, 0, c.RunningCount())
}

// Finding 3: On parent cancel the OnShutdown callback runs asynchronously in the container's own goroutine.
// WaitAllStopped does not wait for the callback, a caller has to wait on its own signal.
func TestOnShutdownParentCancel_Async(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Run(returnCtxErr).Register(c)
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	defer release()
	callbackDone := make(chan struct{})
	callbackStarted := make(chan struct{})
	c.OnShutdown(func() {
		close(callbackStarted)
		<-gate
		close(callbackDone)
	})

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()

	waitSignal(t, callbackStarted, "OnShutdown start")
	// Services are stopped while the callback still blocks: WaitAllStopped does not wait for the callback.
	mustStop(t, c)
	select {
	case <-callbackDone:
		t.Fatal("callback finished before it was released")
	default:
	}
	release()
	waitSignal(t, callbackDone, "OnShutdown done")
}

// Finding 3: after parent cancel, ServiceErrors contains the context error returned by services (pinned behavior),
// services returning nil do not appear. Callers that only want real failures must filter context errors.
func TestServiceErrorsAfterParentCancel(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("clean").Run(waitCtx).Register(c)
	service.New("ctxerr").Run(returnCtxErr).Register(c)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()
	mustStop(t, c)

	errs := c.ServiceErrors()
	assert.NotContains(t, errs, "/clean")
	require.Contains(t, errs, "/ctxerr")
	assert.ErrorIs(t, errs["/ctxerr"], context.Canceled)
	// The stop cause is the intended parent cancel, not a failure
	assert.False(t, isUnexpectedStop(c.StopCause()))
}

// Finding 3: services returning nil after parent cancel produce no ServiceErrors at all.
func TestServiceErrorsAfterParentCancel_CleanServices(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("a").Run(waitCtx).Register(c)
	service.New("b").Run(waitCtx).Register(c)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()
	mustStop(t, c)
	assert.Empty(t, c.ServiceErrors())
}

// containerLevelMessages are lifecycle messages that describe the whole container and have no service attribute.
var containerLevelMessages = map[string]bool{
	"All services started":  true,
	"All services stopped":  true,
	"Stopping all services": true,
	"Container is stopping, skip starting remaining services": true,
	"Services not started because of init failure":            true,
}

// logRecords decodes the JSON log lines
func logRecords(t *testing.T, logs *logBuffer) []map[string]any {
	t.Helper()
	logs.mu.Lock()
	defer logs.mu.Unlock()
	var records []map[string]any
	dec := json.NewDecoder(bytes.NewReader(logs.buf.Bytes()))
	for dec.More() {
		var m map[string]any
		require.NoError(t, dec.Decode(&m))
		records = append(records, m)
	}
	return records
}

// Finding 4: all lifecycle log lines use the key "service", never "name". The real JSON handler is used,
// so a group prefix would show up as nested object.
func TestLogAttr_ServiceKeyNotName(t *testing.T) {
	boom := func(ctx context.Context) error { panic("boom") }
	fail := func(ctx context.Context) error { return errors.New("failed") }
	tests := []struct {
		name string
		// setup registers the services and returns the context for StartAll
		setup func(c *service.Container) (context.Context, context.CancelFunc)
		// messages that must have been logged with a service attribute
		want []string
	}{
		{"healthy start and StopAll", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("db").Init(func(ctx context.Context) error { return nil }).Run(waitCtx).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Registered service", "Initializing service", "Initialized service", "Starting service", "Service stopped"}},
		{"init error", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("db").Init(func(ctx context.Context) error { return nil }).Run(waitCtx).Register(c)
			service.New("bad").Init(func(ctx context.Context) error { return errors.New("nope") }).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Failed to initialize service"}},
		{"init panic", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("bad").Init(boom).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Service panicked during init"}},
		{"init aborted by parent cancel", func(c *service.Container) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			service.New("slow").Init(func(ctx context.Context) error {
				cancel()
				<-ctx.Done()
				return ctx.Err()
			}).Register(c)
			return ctx, cancel
		}, []string{"Service init aborted"}},
		{"run panic with follow-up error", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("parser").Run(boom).Register(c)
			service.New("other").Run(func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("follow-up")
			}).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Service panicked", "Service stopped with error during shutdown"}},
		{"run error", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("worker").Run(fail).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Service failed"}},
		{"unexpected stop", func(c *service.Container) (context.Context, context.CancelFunc) {
			service.New("worker").Run(func(ctx context.Context) error { return nil }).Register(c)
			service.New("http").Run(waitCtx).Register(c)
			return context.WithCancel(context.Background())
		}, []string{"Service stopped unexpectedly, Run() returned nil before the container was stopped. Other services keep running."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &logBuffer{}
			c := service.NewContainer(service.WithName("main"))
			c.SetLogger(slog.New(slog.NewJSONHandler(logs, nil)))
			ctx, cancel := tt.setup(c)
			defer cancel()
			_ = c.StartAll(ctx)
			if c.StopCause() == nil {
				time.Sleep(20 * time.Millisecond)
				c.StopAll()
			}
			mustStop(t, c)

			seen := map[string]bool{}
			for _, r := range logRecords(t, logs) {
				msg := r["msg"].(string)
				assert.NotContains(t, r, "name", "message %q uses key name", msg)
				assert.Contains(t, r, "container", "message %q has no container attr", msg)
				if containerLevelMessages[msg] {
					continue
				}
				assert.IsType(t, "", r["service"], "message %q misses the service attribute", msg)
				assert.NotEmpty(t, r["service"], "message %q has an empty service attribute", msg)
				seen[msg] = true
			}
			for _, msg := range tt.want {
				assert.True(t, seen[msg], "message %q was not logged", msg)
			}
		})
	}
}

// Finding 5: StartAll error wraps the original error and names the service.
func TestStartAllError_ContainsCause(t *testing.T) {
	initErr := errors.New("dial tcp: connection refused")
	c, _ := newLoggedContainer(t)
	service.New("mqtt").Init(func(ctx context.Context) error { return initErr }).Run(waitCtx).Register(c)
	err := c.StartAll(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, initErr)
	assert.Contains(t, err.Error(), "mqtt")
	assert.Contains(t, err.Error(), "connection refused")
}

// Finding 5: also for an init panic.
func TestStartAllError_ContainsCause_InitPanic(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("mqtt").Init(func(ctx context.Context) error { panic("nil map") }).Run(waitCtx).Register(c)
	err := c.StartAll(context.Background())
	require.Error(t, err)
	var panicErr *service.PanicError
	require.ErrorAs(t, err, &panicErr)
	assert.Equal(t, "nil map", panicErr.Value)
	assert.Contains(t, err.Error(), "mqtt")
	assert.Contains(t, err.Error(), "nil map")
}

// Finding 5: a custom error type survives errors.As through StartAll.
type initFailure struct{ code int }

func (e *initFailure) Error() string { return "init failure" }

func TestStartAllError_ContainsCause_ErrorsAs(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Init(func(ctx context.Context) error { return &initFailure{code: 7} }).Register(c)
	err := c.StartAll(context.Background())
	var f *initFailure
	require.ErrorAs(t, err, &f)
	assert.Equal(t, 7, f.code)
	assert.Contains(t, err.Error(), "db")
}
