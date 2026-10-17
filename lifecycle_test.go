package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logBuffer collects log output in a goroutine-safe way
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSpace(b.buf.String()), "\n")
}

// grep returns all log lines containing all given substrings
func (b *logBuffer) grep(subs ...string) []string {
	var res []string
	for _, l := range b.lines() {
		match := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				match = false
				break
			}
		}
		if match {
			res = append(res, l)
		}
	}
	return res
}

func newLoggedContainer(t *testing.T) (*service.Container, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	c := service.NewContainer()
	c.SetLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("logs:\n%s", strings.Join(logs.lines(), "\n"))
		}
	})
	return c, logs
}

func waitCtx(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func returnCtxErr(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// S1/S13: Stop via parent context, services return nil
func TestStopCause_ParentContext(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	service.New("http").Run(waitCtx).Register(c)
	shutdownCalled := make(chan struct{})
	c.OnShutdown(func() { close(shutdownCalled) })

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()
	waitStopped(c, time.Second)

	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("OnShutdown not called on parent context cancel")
	}

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonParentContext, stopErr.Reason)
	assert.ErrorIs(t, c.StopCause(), context.Canceled)
	assert.Len(t, logs.grep("Stopping all services"), 1)
	assert.Len(t, logs.grep("level=INFO", "Stopping all services", `reason="parent context done"`), 1)
	assert.Empty(t, logs.grep("level=ERROR"))
	assert.Len(t, logs.grep("All services started", "count=2"), 1)
	assert.Len(t, logs.grep("All services stopped", "count=2", "failed=0"), 1)
}

// S14: Clean shutdown via parent context where services return ctx.Err() must not log errors
func TestStopCause_ParentContext_ServicesReturnCtxErr(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Run(returnCtxErr).Register(c)
	service.New("http").Run(returnCtxErr).Register(c)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.StartAll(ctx))
	cancel()
	waitStopped(c, time.Second)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonParentContext, stopErr.Reason)
	assert.Empty(t, logs.grep("level=ERROR"))
	assert.Empty(t, logs.grep("level=WARN"))
	assert.Len(t, logs.grep("Stopping all services"), 1)
	assert.Len(t, logs.grep("level=INFO", "Service stopped", "error=\"context canceled\""), 2)
}

// S5: External StopAll
func TestStopCause_StopAll(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	c.StopAll()
	waitStopped(c, time.Second)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonStopAll, stopErr.Reason)
	assert.Len(t, logs.grep("level=INFO", "Stopping all services", `reason="StopAll called"`), 1)
	assert.Empty(t, logs.grep("level=ERROR"))
}

// S3/S11: Root cause is the only ERROR, follow-up errors are logged as such
func TestFollowUpErrors(t *testing.T) {
	c, logs := newLoggedContainer(t)
	rootErr := errors.New("broker closed connection")
	failNow := make(chan struct{})
	service.New("db").Run(returnCtxErr).Register(c)
	service.New("mqtt").Run(func(ctx context.Context) error {
		<-failNow
		return rootErr
	}).Register(c)
	service.New("http").Run(func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("listener closed")
	}).Register(c)

	require.NoError(t, c.StartAll(context.Background()))
	close(failNow)
	waitStopped(c, time.Second)

	errorLines := logs.grep("level=ERROR")
	require.Len(t, errorLines, 1)
	assert.Contains(t, errorLines[0], `msg="Service failed" container="" service=mqtt error="broker closed connection"`)
	assert.Len(t, logs.grep("level=WARN", "Stopping all services", `reason="service failed"`, "service=mqtt"), 1)
	assert.Len(t, logs.grep("level=WARN", "Service stopped with error during shutdown", "service=http", "stop_cause="), 1)
	assert.Len(t, logs.grep("level=INFO", "Service stopped", "service=db", `error="context canceled"`), 1)
	assert.Len(t, logs.grep("All services stopped", "count=3", "failed=2"), 1)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonServiceFailed, stopErr.Reason)
	assert.Equal(t, "mqtt", stopErr.Service)
	assert.ErrorIs(t, c.StopCause(), rootErr)
	// ServiceErrors still contains all errors
	assert.Len(t, c.ServiceErrors(), 3)
}

// S2: Init failure is visible on ERROR level and lists the services that were not started
func TestInitFailure(t *testing.T) {
	c, logs := newLoggedContainer(t)
	initErr := errors.New("dial tcp 10.0.0.5:1883: connection refused")
	service.New("db").Init(func(ctx context.Context) error { return nil }).Run(waitCtx).Register(c)
	service.New("mqtt").Init(func(ctx context.Context) error { return initErr }).Run(waitCtx).Register(c)
	service.New("http").Run(waitCtx).Register(c)
	shutdownCalled := false
	c.OnShutdown(func() { shutdownCalled = true })

	err := c.StartAll(context.Background())
	require.ErrorIs(t, err, initErr)
	waitStopped(c, time.Second)

	assert.Len(t, logs.grep("level=ERROR", "Failed to initialize service", "service=mqtt", "connection refused"), 1)
	assert.Len(t, logs.grep("level=WARN", "Services not started because of init failure", "initialized=[db]", "not_started=\"[db http]\""), 1)
	assert.Len(t, logs.grep("level=WARN", "Stopping all services", `reason="service init failed"`, "service=mqtt"), 1)
	assert.Empty(t, logs.grep("Starting service"))
	assert.True(t, shutdownCalled)
	assert.Len(t, logs.grep("All services stopped", "count=0"), 1)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonInitFailed, stopErr.Reason)
	assert.Equal(t, "mqtt", stopErr.Service)
	assert.ErrorIs(t, c.StopCause(), initErr)
	// ServiceErrors only contains Run errors (unchanged behaviour)
	assert.Empty(t, c.ServiceErrors())
}

// S6: A service returning nil before the container stops is logged as warning
func TestUnexpectedStop(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("worker").Run(func(ctx context.Context) error { return nil }).Register(c)
	service.New("http").Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	require.Eventually(t, func() bool { return c.RunningCount() == 1 }, time.Second, time.Millisecond)

	assert.Len(t, logs.grep("level=WARN", "Service stopped unexpectedly", "service=worker"), 1)
	assert.NoError(t, c.StopCause(), "container keeps running")
	assert.Equal(t, []string{"http"}, c.RunningServiceNames())

	c.StopAll()
	waitStopped(c, time.Second)
}

// S7: Timeout while waiting logs the services that are still running
func TestWaitTimeoutLogsRunningServices(t *testing.T) {
	c, logs := newLoggedContainer(t)
	release := make(chan struct{})
	defer close(release)
	service.New("db").Run(waitCtx).Register(c)
	service.New("stuck").Run(func(ctx context.Context) error {
		<-ctx.Done()
		<-release
		return nil
	}).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	waitStopped(c, 100*time.Millisecond)

	assert.Equal(t, []string{"stuck"}, c.RunningServiceNames())
	assert.Len(t, logs.grep("level=WARN", "Timeout waiting for services to stop", "running=[stuck]"), 1)
	assert.Empty(t, logs.grep("All services stopped"))
}

// S8: A panic in Run is recovered, logged with service name and stops the container
func TestPanicInRun(t *testing.T) {
	c, logs := newLoggedContainer(t)
	db := &testService{Name: "db"}
	c.Register(db)
	c.Register(&testService{Name: "parser", PanicDuringRun: "assignment to entry in nil map"})
	require.NoError(t, c.StartAll(context.Background()))
	waitStopped(c, time.Second)

	assertServiceStartedAndStopped(t, db)
	assert.Len(t, logs.grep("level=ERROR", "Service panicked", "service=testService.parser", "stack="), 1)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonPanic, stopErr.Reason)
	assert.Equal(t, "testService.parser", stopErr.Service)
	var panicErr *service.PanicError
	require.ErrorAs(t, c.StopCause(), &panicErr)
	assert.Equal(t, "assignment to entry in nil map", panicErr.Value)
	require.ErrorAs(t, c.ServiceErrors()["/testService.parser"], &panicErr)
}

// A panic in Init is recovered and reported like an init error
func TestPanicInInit(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Init(func(ctx context.Context) error { panic("boom") }).Run(waitCtx).Register(c)
	err := c.StartAll(context.Background())
	var panicErr *service.PanicError
	require.ErrorAs(t, err, &panicErr)
	assert.Len(t, logs.grep("level=ERROR", "Service panicked during init", "service=db"), 1)
	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonPanic, stopErr.Reason)
}

// S12: Once a service failed, StartAll does not start further services
func TestNoStartAfterFailure(t *testing.T) {
	c, logs := newLoggedContainer(t)
	failed := make(chan struct{})
	service.New("s00").Run(func(ctx context.Context) error {
		defer close(failed)
		return errors.New("s00 failed immediately")
	}).Register(c)
	// Init of s01 runs before any Run, so use Run of s01 to wait for the failure of s00.
	// s02 is started after s01 returned from runOne, by then s00 is guaranteed to have failed.
	service.New("s01").Run(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}).Register(c)
	for i := 2; i < 40; i++ {
		service.New(fmt.Sprintf("s%02d", i)).Run(waitCtx).Register(c)
	}
	c.OnShutdown(func() {})

	if err := c.StartAll(context.Background()); err != nil {
		// Start was aborted because s00 failed while services were still started
		assert.ErrorIs(t, err, c.StopCause())
	}
	<-failed
	waitStopped(c, time.Second)

	// No service is started after the shutdown began
	lines := logs.lines()
	stopIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "Stopping all services") {
			stopIdx = i
		}
		if stopIdx >= 0 && i > stopIdx {
			assert.NotContains(t, l, "Starting service", fmt.Sprintf("line %d", i))
		}
	}
	require.GreaterOrEqual(t, stopIdx, 0)
	assert.Equal(t, 0, c.RunningCount())
}

// Builder services without init function do not log init messages
func TestNoInitLogsWithoutInit(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("noinit").Run(waitCtx).Register(c)
	c.Register(service.WithRunFunc(waitCtx))
	service.New("withinit").Init(func(ctx context.Context) error { return nil }).Run(waitCtx).Register(c)
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	waitStopped(c, time.Second)

	assert.Len(t, logs.grep("Initializing service"), 1)
	assert.Len(t, logs.grep("Initializing service", "service=withinit"), 1)
}

// A parent context that is done before StartAll runs the services aborts the start
func TestStartAbortedOnParentContextDone(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.StartAll(ctx)
	require.ErrorIs(t, err, context.Canceled)
	var stopErr *service.StopError
	require.ErrorAs(t, err, &stopErr)
	assert.Equal(t, service.StopReasonParentContext, stopErr.Reason)
	assert.Empty(t, logs.grep("Starting service"))
	assert.Empty(t, logs.grep("All services started"))
	assert.Len(t, logs.grep("Container is stopping, skip starting remaining services", "skipped=[db]"), 1)
}

// An OnShutdown callback may wait for the services to stop (regression: recursive sync.Once deadlock)
func TestOnShutdownCallbackCanWait(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Run(func(ctx context.Context) error { return errors.New("failed") }).Register(c)
	waited := make(chan struct{})
	c.OnShutdown(func() {
		waitStopped(c, time.Second)
		close(waited)
	})
	_ = c.StartAll(context.Background())
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: OnShutdown callback waiting for services never returned")
	}
}

// A service returning nil while shutdown callbacks run is not reported as unexpected stop
func TestNoUnexpectedStopDuringCallbacks(t *testing.T) {
	c, logs := newLoggedContainer(t)
	release := make(chan struct{})
	service.New("worker").Run(func(ctx context.Context) error {
		<-release
		return nil
	}).Register(c)
	c.OnShutdown(func() {
		close(release)
		require.Eventually(t, func() bool { return c.RunningCount() == 0 }, time.Second, time.Millisecond)
	})
	require.NoError(t, c.StartAll(context.Background()))
	c.StopAll()
	waitStopped(c, time.Second)
	assert.Empty(t, logs.grep("Service stopped unexpectedly"))
	assert.Len(t, logs.grep("level=INFO", "Service stopped", "service=worker"), 1)
}

// Option A: services are canceled before the OnShutdown callbacks run, so a callback can wait for all services
func TestOnShutdownCallbackWaitsForRunningServices(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("mqtt").Run(func(ctx context.Context) error { return errors.New("broker closed connection") }).Register(c)
	service.New("http").Run(returnCtxErr).Register(c)
	service.New("db").Run(waitCtx).Register(c)

	type result struct {
		took time.Duration
		errs map[string]error
	}
	res := make(chan result, 1)
	c.OnShutdown(func() {
		start := time.Now()
		waitStopped(c, time.Second)
		res <- result{time.Since(start), c.ServiceErrors()}
	})
	_ = c.StartAll(context.Background())

	select {
	case r := <-res:
		assert.Less(t, r.took, 500*time.Millisecond, "callback ran into WaitAllStopped timeout")
		assert.Contains(t, r.errs, "/mqtt")
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not finish")
	}
}

// StopAll inside an OnShutdown callback must not deadlock
func TestStopAllInsideOnShutdown(t *testing.T) {
	c, _ := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	done := make(chan struct{})
	c.OnShutdown(func() {
		c.StopAll()
		close(done)
	})
	require.NoError(t, c.StartAll(context.Background()))
	go c.StopAll()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("deadlock: StopAll inside OnShutdown")
	}
	waitStopped(c, time.Second)
	assert.Equal(t, 0, c.RunningCount())
}

// The usual SIGTERM pattern: parent context is canceled and the application calls StopAll right after.
// The stop cause must always be the parent context, independent of goroutine scheduling.
func TestStopAllAfterParentCancel(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Run(waitCtx).Register(c)
	ctx, cancel := context.WithCancelCause(context.Background())
	require.NoError(t, c.StartAll(ctx))
	sigterm := errors.New("SIGTERM")
	cancel(sigterm)
	c.StopAll()
	waitStopped(c, time.Second)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, service.StopReasonParentContext, stopErr.Reason)
	assert.ErrorIs(t, c.StopCause(), sigterm)
	assert.Len(t, logs.grep("Stopping all services", `reason="parent context done"`, "error=SIGTERM"), 1)
}

// The service that caused the stop is always counted as failed, also if its error wraps a context error
func TestStoppedSummaryCountsRootCause(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("query").Run(func(ctx context.Context) error {
		return fmt.Errorf("query: %w", context.DeadlineExceeded)
	}).Register(c)
	_ = c.StartAll(context.Background())
	waitStopped(c, time.Second)
	assert.Len(t, logs.grep("All services stopped", "count=1", "failed=1"), 1)
}

// After an init failure the failing service is counted
func TestStoppedSummaryCountsInitFailure(t *testing.T) {
	c, logs := newLoggedContainer(t)
	service.New("db").Init(func(ctx context.Context) error { return errors.New("no db") }).Run(waitCtx).Register(c)
	require.Error(t, c.StartAll(context.Background()))
	waitStopped(c, time.Second)
	assert.Len(t, logs.grep("All services stopped", "count=0", "failed=1"), 1)
}

// When two services fail at the same time, the root cause is logged before "Stopping all services"
// and it is the only ERROR line
func TestSimultaneousFailuresLogOrder(t *testing.T) {
	c, logs := newLoggedContainer(t)
	start := make(chan struct{})
	for _, name := range []string{"a", "b", "c", "d"} {
		name := name
		service.New(name).Run(func(ctx context.Context) error {
			<-start
			return fmt.Errorf("%s broke", name)
		}).Register(c)
	}
	require.NoError(t, c.StartAll(context.Background()))
	close(start)
	waitStopped(c, time.Second)

	lines := logs.lines()
	failedIdx, stoppingIdx := -1, -1
	for i, l := range lines {
		if strings.Contains(l, `msg="Service failed"`) {
			failedIdx = i
		}
		if strings.Contains(l, "Stopping all services") {
			stoppingIdx = i
		}
		if stoppingIdx < 0 {
			assert.NotContains(t, l, "Service stopped with error during shutdown", "follow-up error logged before stop cause")
		}
	}
	require.GreaterOrEqual(t, failedIdx, 0)
	assert.Less(t, failedIdx, stoppingIdx)
	assert.Len(t, logs.grep("level=ERROR"), 1)
}

// Container name is part of logs and the stop cause
func TestContainerNameInLogs(t *testing.T) {
	logs := &logBuffer{}
	c := service.NewContainer(service.WithName("core"))
	c.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	service.New("db").Run(func(ctx context.Context) error { return errors.New("down") }).Register(c)
	_ = c.StartAll(context.Background())
	waitStopped(c, time.Second)

	var stopErr *service.StopError
	require.ErrorAs(t, c.StopCause(), &stopErr)
	assert.Equal(t, "core", stopErr.Container)
	assert.Len(t, logs.grep("level=ERROR", "container=core", "service=db"), 1)
	assert.Contains(t, c.ServiceErrors(), "core/db")
}
