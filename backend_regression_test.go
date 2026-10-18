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

// isUnexpectedStop is what a backend main() uses to derive a non-zero exit code from StopCause().
func isUnexpectedStop(cause error) bool {
	var stopErr *service.StopError
	if !errors.As(cause, &stopErr) {
		return false
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
			waitStopped(c, time.Second)

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
	// parent cancel
	c, _ := newLoggedContainer(t)
	service.New("db").Run(returnCtxErr).Register(c)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()
	waitStopped(c, time.Second)
	parentCause := c.StopCause()

	// StopAll
	c2, _ := newLoggedContainer(t)
	service.New("db").Run(returnCtxErr).Register(c2)
	require.NoError(t, c2.StartAll(context.Background()))
	c2.StopAll()
	waitStopped(c2, time.Second)
	stopAllCause := c2.StopCause()

	// panic
	c3, _ := newLoggedContainer(t)
	service.New("db").Run(func(ctx context.Context) error { panic("boom") }).Register(c3)
	require.NoError(t, c3.StartAll(context.Background()))
	waitStopped(c3, time.Second)
	panicCause := c3.StopCause()

	assert.False(t, isUnexpectedStop(parentCause))
	assert.False(t, isUnexpectedStop(stopAllCause))
	assert.True(t, isUnexpectedStop(panicCause))

	var pe *service.PanicError
	assert.False(t, errors.As(parentCause, &pe))
	assert.False(t, errors.As(stopAllCause, &pe))
	assert.True(t, errors.As(panicCause, &pe))
}

// Finding 1/2: a service returning an error and one returning nil early are also unexpected stops or not stops at all.
func TestStopCause_NilBeforeStopIsNoStop(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("worker").Run(func(ctx context.Context) error { return nil }).Register(c)
	service.New("http").Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	require.Eventually(t, func() bool { return c.RunningCount() == 1 }, time.Second, time.Millisecond)
	assert.NoError(t, c.StopCause())
	c.StopAll()
	waitStopped(c, time.Second)
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
			waitStopped(c, time.Second)
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
			waitStopped(c, time.Second)
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
	waitStopped(c, time.Second)

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
	waitStopped(c, time.Second)
	assert.Equal(t, 0, c.RunningCount())
}

// Finding 3: On parent cancel the OnShutdown callback runs asynchronously in the container's own goroutine.
// WaitAllStopped does not wait for the callback, a caller has to wait on its own signal.
func TestOnShutdownParentCancel_Async(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Run(returnCtxErr).Register(c)
	gate := make(chan struct{})
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
	waitStopped(c, time.Second)
	select {
	case <-callbackDone:
		t.Fatal("callback finished before it was released")
	default:
	}
	close(gate)
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
	waitStopped(c, time.Second)

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
	waitStopped(c, time.Second)
	assert.Empty(t, c.ServiceErrors())
}

type recordHandler struct {
	mu      *sync.Mutex
	records *[]map[string]any
	attrs   []slog.Attr
}

func (h recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordHandler) Handle(_ context.Context, r slog.Record) error {
	m := map[string]any{"msg": r.Message}
	for _, a := range h.attrs {
		m[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, m)
	return nil
}
func (h recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return h
}
func (h recordHandler) WithGroup(string) slog.Handler { return h }

// Finding 4: lifecycle log lines use the key "service", never "name".
func TestLogAttr_ServiceKeyNotName(t *testing.T) {
	var mu sync.Mutex
	var records []map[string]any
	c := service.NewContainer(service.WithName("main"))
	c.SetLogger(slog.New(recordHandler{mu: &mu, records: &records}))

	service.New("db").Init(func(ctx context.Context) error { return nil }).Run(returnCtxErr).Register(c)
	service.New("parser").Run(func(ctx context.Context) error { panic("boom") }).Register(c)
	_ = c.StartAll(context.Background())
	waitStopped(c, time.Second)

	c3 := service.NewContainer(service.WithName("run"))
	c3.SetLogger(slog.New(recordHandler{mu: &mu, records: &records}))
	service.New("worker").Run(func(ctx context.Context) error { return errors.New("failed") }).Register(c3)
	_ = c3.StartAll(context.Background())
	waitStopped(c3, time.Second)

	c2 := service.NewContainer(service.WithName("init"))
	c2.SetLogger(slog.New(recordHandler{mu: &mu, records: &records}))
	service.New("bad").Init(func(ctx context.Context) error { panic("boom") }).Register(c2)
	_ = c2.StartAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, records)
	perMessage := map[string]bool{}
	for _, r := range records {
		assert.NotContains(t, r, "name", "message %q uses key name", r["msg"])
		assert.Contains(t, r, "container", "message %q has no container attr", r["msg"])
		if s, ok := r["service"]; ok {
			assert.NotEmpty(t, s)
			perMessage[r["msg"].(string)] = true
		}
	}
	for _, msg := range []string{"Registered service", "Initializing service", "Initialized service", "Starting service",
		"Service panicked", "Service failed", "Service panicked during init"} {
		assert.True(t, perMessage[msg], "message %q misses the service attribute", msg)
	}
}

// Finding 4: same check with the real JSON handler, so the key is visible in the actual output.
func TestLogAttr_ServiceKeyJSON(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	c := service.NewContainer(service.WithName("main"))
	c.SetLogger(slog.New(slog.NewJSONHandler(&lockedWriter{mu: &mu, w: &buf}, nil)))
	service.New("db").Init(func(ctx context.Context) error { return nil }).Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	waitStopped(c, time.Second)

	mu.Lock()
	defer mu.Unlock()
	dec := json.NewDecoder(&buf)
	found := 0
	for dec.More() {
		var m map[string]any
		require.NoError(t, dec.Decode(&m))
		assert.NotContains(t, m, "name")
		if m["msg"] == "Starting service" {
			assert.Equal(t, "db", m["service"])
			found++
		}
	}
	assert.Equal(t, 1, found)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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
