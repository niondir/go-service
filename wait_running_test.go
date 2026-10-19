package service_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readyService runs until ctx is done and becomes ready when ready is closed.
type readyService struct {
	name   string
	ready  chan struct{}
	runErr chan error // optional: a value makes Run return it
}

func (s *readyService) String() string { return s.name }

func (s *readyService) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.runErr:
		return err
	}
}

func (s *readyService) WaitReady(timeout time.Duration) bool {
	select {
	case <-s.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

func newReadyService(name string) *readyService {
	return &readyService{name: name, ready: make(chan struct{}), runErr: make(chan error, 1)}
}

func startContainer(t *testing.T, ctx context.Context, services ...service.Runner) *service.Container {
	t.Helper()
	c := service.NewContainer(service.WithName("test"))
	for _, s := range services {
		c.Register(s)
	}
	require.NoError(t, c.StartAll(ctx))
	t.Cleanup(func() {
		c.StopAll()
		c.WaitAllStopped(context.Background())
	})
	return c
}

func TestWaitAllRunning_Ready(t *testing.T) {
	a, b := newReadyService("a"), newReadyService("b")
	c := startContainer(t, context.Background(), a, b)
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(a.ready)
		close(b.ready)
	}()
	assert.NoError(t, c.WaitAllRunning(context.Background(), 5*time.Second))
}

func TestWaitAllRunning_NoReadyWaiterIsReady(t *testing.T) {
	c := startContainer(t, context.Background(), service.WithRunFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}))
	assert.NoError(t, c.WaitAllRunning(context.Background(), time.Second))
}

func TestWaitAllRunning_Timeout(t *testing.T) {
	c := startContainer(t, context.Background(), newReadyService("slow"))
	start := time.Now()
	err := c.WaitAllRunning(context.Background(), 100*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrWaitRunningTimeout)
	assert.Contains(t, err.Error(), "slow")
	var se *service.StopError
	assert.False(t, errors.As(err, &se), "a timeout is not a stop")
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Nil(t, c.StopCause(), "waiting does not stop the container")
}

func TestWaitAllRunning_ContextDone(t *testing.T) {
	c := startContainer(t, context.Background(), newReadyService("slow"))
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("caller gave up")
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel(cause)
	}()
	start := time.Now()
	err := c.WaitAllRunning(ctx, time.Minute)
	assert.ErrorIs(t, err, cause)
	assert.Less(t, time.Since(start), 5*time.Second, "must not wait for the timeout")
	assert.Nil(t, c.StopCause())
}

func TestWaitAllRunning_ParentContextDone(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	c := startContainer(t, ctx, newReadyService("slow"))
	shutdown := errors.New("shutdown")
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel(shutdown)
	}()
	err := c.WaitAllRunning(ctx, time.Minute)
	var se *service.StopError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, service.StopReasonParentContext, se.Reason)
	assert.ErrorIs(t, err, shutdown)
}

func TestWaitAllRunning_StopAll(t *testing.T) {
	c := startContainer(t, context.Background(), newReadyService("slow"))
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.StopAll()
	}()
	err := c.WaitAllRunning(context.Background(), time.Minute)
	var se *service.StopError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, service.StopReasonStopAll, se.Reason)
}

func TestWaitAllRunning_AlreadyStopped(t *testing.T) {
	c := startContainer(t, context.Background(), newReadyService("slow"))
	c.StopAll()
	var se *service.StopError
	require.ErrorAs(t, c.WaitAllRunning(context.Background(), time.Minute), &se)
	assert.Equal(t, service.StopReasonStopAll, se.Reason)
}

// The failing service ends its wait before the container finished to stop.
// The result must be the service error, never the timeout. Repeated to catch the ordering race.
func TestWaitAllRunning_ServiceFailureWinsOverTimeout(t *testing.T) {
	boom := errors.New("boom")
	for i := 0; i < 50; i++ {
		failing, other := newReadyService("failing"), newReadyService("other")
		c := startContainer(t, context.Background(), failing, other)
		// Timeout is already (nearly) elapsed when the service fails: the cause must still win.
		go func() {
			time.Sleep(10 * time.Millisecond)
			failing.runErr <- boom
		}()
		err := c.WaitAllRunning(context.Background(), 10*time.Millisecond)
		var se *service.StopError
		require.ErrorAs(t, err, &se, "iteration %d: %v", i, err)
		assert.NotErrorIs(t, err, service.ErrWaitRunningTimeout)
		assert.Equal(t, service.StopReasonServiceFailed, se.Reason)
		assert.Equal(t, "failing", se.Service)
		assert.ErrorIs(t, err, boom)
	}
}

func TestWaitAllRunning_ServiceFailureWhileWaiting(t *testing.T) {
	boom := errors.New("boom")
	failing := newReadyService("failing")
	c := startContainer(t, context.Background(), failing, newReadyService("other"))
	go func() {
		time.Sleep(30 * time.Millisecond)
		failing.runErr <- boom
	}()
	start := time.Now()
	err := c.WaitAllRunning(context.Background(), time.Minute)
	var se *service.StopError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, "failing", se.Service)
	assert.ErrorIs(t, err, boom)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestWaitAllRunning_InitFailure(t *testing.T) {
	boom := errors.New("init boom")
	c := service.NewContainer(service.WithName("test"))
	c.Register(service.WithFunc(func(ctx context.Context) error { return boom }, func(ctx context.Context) error { return nil }))
	require.Error(t, c.StartAll(context.Background()))
	var se *service.StopError
	err := c.WaitAllRunning(context.Background(), time.Minute)
	require.ErrorAs(t, err, &se)
	assert.Equal(t, service.StopReasonInitFailed, se.Reason)
	assert.ErrorIs(t, err, boom)
}

func TestWaitAllRunning_NoStartAllPanics(t *testing.T) {
	c := service.NewContainer()
	assert.Panics(t, func() { _ = c.WaitAllRunning(context.Background(), time.Second) })
}

func TestWaitAllRunning_NoLeakedGoroutines(t *testing.T) {
	services := make([]service.Runner, 0, 10)
	for i := 0; i < 10; i++ {
		services = append(services, newReadyService(string(rune('a'+i))))
	}
	c := startContainer(t, context.Background(), services...)
	before := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		require.ErrorIs(t, c.WaitAllRunning(context.Background(), 30*time.Millisecond), service.ErrWaitRunningTimeout)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		require.Error(t, c.WaitAllRunning(ctx, time.Minute))
		cancel()
	}
	// WaitAllRunning joins its goroutines, so no waiting is needed. Allow the runtime to retire exited goroutines.
	var after int
	for i := 0; i < 20; i++ {
		after = runtime.NumGoroutine()
		if after <= before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.LessOrEqual(t, after, before, "goroutines leaked by WaitAllRunning")
}

// slowWaitService returns from WaitReady only after the wait timeout elapsed, and fails in between.
type slowWaitService struct {
	failAfter time.Duration
	waitFor   time.Duration
	err       error
}

func (s *slowWaitService) String() string { return "slow-wait" }

func (s *slowWaitService) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(s.failAfter):
		return s.err
	}
}

func (s *slowWaitService) WaitReady(time.Duration) bool {
	time.Sleep(s.waitFor)
	return false
}

// Order: the timeout elapses (20ms), the service fails (60ms), then WaitReady returns (150ms).
// The timeout was first, but the service error is the cause and must be returned.
func TestWaitAllRunning_FailureAfterTimeoutStillWins(t *testing.T) {
	boom := errors.New("boom")
	c := startContainer(t, context.Background(), &slowWaitService{failAfter: 60 * time.Millisecond, waitFor: 150 * time.Millisecond, err: boom})
	err := c.WaitAllRunning(context.Background(), 20*time.Millisecond)
	var se *service.StopError
	require.ErrorAs(t, err, &se)
	assert.NotErrorIs(t, err, service.ErrWaitRunningTimeout)
	assert.Equal(t, service.StopReasonServiceFailed, se.Reason)
	assert.ErrorIs(t, err, boom)
}
