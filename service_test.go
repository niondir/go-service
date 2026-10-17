package service_test

import (
	"context"
	"fmt"
	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var _ service.Initer = &testService{}
var _ service.Runner = &testService{}
var _ fmt.Stringer = &testService{}

// testService is a service that tracks it's state to be checked in tests
type testService struct {
	Name string
	// An error that will be returned in init
	ErrorDuringInit error
	// An error that will be returned during run
	ErrorDuringRun error
	// An error that will be returned when the service shut down
	ErrorAfterRun error
	// If set the service will not wait for <-ctx.Done()
	SkipWaitForCtx bool
	// If set, Run panics with this value
	PanicDuringRun any
	// If set, Run blocks this long after <-ctx.Done() before returning
	ShutdownDelay time.Duration

	mu          sync.Mutex
	initialized bool
	started     bool
	running     bool
	stopped     bool
	err         error
	startedCh   chan struct{}
}

type testServiceState struct {
	initialized, started, running, stopped bool
	err                                    error
}

func (t *testService) String() string {
	return fmt.Sprintf("testService.%s", t.Name)
}

func (t *testService) state() testServiceState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return testServiceState{t.initialized, t.started, t.running, t.stopped, t.err}
}

func (t *testService) Init(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.initialized {
		return fmt.Errorf("service %s was already initialized", t.Name)
	}
	if t.ErrorDuringInit != nil {
		return t.ErrorDuringInit
	}
	t.startedCh = make(chan struct{})
	t.initialized = true
	return nil
}

func (t *testService) Run(ctx context.Context) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return fmt.Errorf("service %s already running", t.Name)
	}
	t.running = true
	if t.started {
		t.mu.Unlock()
		return fmt.Errorf("service %s was already started", t.Name)
	}
	t.started = true
	if t.startedCh != nil {
		close(t.startedCh)
	}

	if t.PanicDuringRun != nil {
		t.mu.Unlock()
		panic(t.PanicDuringRun)
	}

	if t.ErrorDuringRun != nil {
		t.running = false
		t.stopped = true
		t.mu.Unlock()
		return t.ErrorDuringRun
	}
	t.mu.Unlock()

	if !t.SkipWaitForCtx {
		<-ctx.Done()
		time.Sleep(t.ShutdownDelay)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false

	if t.stopped {
		return fmt.Errorf("service %s was already stopped", t.Name)
	}
	t.stopped = true

	return t.ErrorAfterRun
}

// waitStopped waits until all services of c stopped or the timeout is reached
func waitStopped(c *service.Container, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c.WaitAllStopped(ctx)
}

func assertServiceStartedAndStopped(t *testing.T, s *testService) {
	t.Helper()
	st := s.state()
	assert.True(t, st.initialized, "initialized")
	assert.True(t, st.started, "started")
	assert.True(t, st.stopped, "stopped")
	assert.False(t, st.running, "running")
	assert.NoError(t, st.err, "err")
}

func assertServiceStillRunning(t *testing.T, s *testService) {
	t.Helper()
	st := s.state()
	assert.True(t, st.initialized)
	assert.True(t, st.started)
	assert.False(t, st.stopped, "Stopped")
	assert.True(t, st.running, "Still Running")
	assert.NoError(t, st.err)
}

func assertServiceOnlyInitialized(t *testing.T, s *testService) {
	t.Helper()
	st := s.state()
	assert.True(t, st.initialized)
	assert.False(t, st.started)
	assert.False(t, st.stopped)
	assert.False(t, st.running)
	assert.NoError(t, st.err)
}

func assertServiceNeverStarted(t *testing.T, s *testService) {
	t.Helper()
	st := s.state()
	assert.False(t, st.initialized)
	assert.False(t, st.started)
	assert.False(t, st.stopped)
	assert.False(t, st.running)
	assert.NoError(t, st.err)
}

func TestStartAndStopWithContext(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	ctx, cancelCtx := context.WithCancel(context.Background())
	err := c.StartAll(ctx)
	require.NoError(t, err)

	cancelCtx()
	c.WaitAllStopped(context.Background())
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
}

func TestStartAndStopWithContext_timeout(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	err := c.StartAll(context.Background())
	require.NoError(t, err)

	waitStopped(c, 100*time.Millisecond)
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStillRunning(t, s1)
}

// Start and Stop multiple services (happy path)
func TestStartAndStop(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	s2 := &testService{
		Name: "s2",
	}
	c.Register(s2)

	err := c.StartAll(context.Background())
	require.NoError(t, err)

	c.StopAll()
	c.WaitAllStopped(context.Background())
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
}

// Start 3 services, the second will just return but the other two will keep running
func TestServiceCanReturnWithoutError(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	s2 := &testService{
		Name:           "s2",
		SkipWaitForCtx: true,
	}
	c.Register(s2)

	s3 := &testService{
		Name: "s3",
	}
	c.Register(s3)

	err := c.StartAll(context.Background())
	require.NoError(t, err)

	// wait all started
	<-s1.startedCh
	<-s2.startedCh
	<-s3.startedCh
	require.Eventually(t, func() bool { return s2.state().stopped }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return c.RunningCount() == 2 }, time.Second, time.Millisecond)
	assertServiceStillRunning(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStillRunning(t, s3)

	assert.Len(t, c.ServiceNames(), 3)
	assert.Equal(t, 2, c.RunningCount())

	c.StopAll()
	c.WaitAllStopped(context.Background())
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s2)
}

// Start 3 services, the second fails during init, none should run
func TestStopWhenInitFails(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	s2 := &testService{
		Name:            "s2",
		ErrorDuringInit: fmt.Errorf("service failed during init"),
	}
	c.Register(s2)

	s3 := &testService{
		Name: "s3",
	}
	c.Register(s3)

	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()
	err := c.StartAll(runCtx)
	require.Error(t, err)

	// Expect all services to stop, since there was an error
	c.WaitAllStopped(context.Background())
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceOnlyInitialized(t, s1)
	assertServiceNeverStarted(t, s2)
	assertServiceNeverStarted(t, s3)
}

// Start 3 services, the second fails during run
func TestStopWhenRunFails(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	s2 := &testService{
		Name:           "s2",
		ErrorDuringRun: fmt.Errorf("service failed during run"),
	}
	c.Register(s2)

	s3 := &testService{
		Name: "s3",
	}
	c.Register(s3)

	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()
	err := c.StartAll(runCtx)
	require.NoError(t, err)

	// Expect all services to stop, since there was an error
	c.WaitAllStopped(context.Background())

	require.Len(t, c.ServiceErrors(), 1)
	errs := c.ServiceErrors()

	assert.NotNil(t, errs["/"+s2.String()])

	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s3)
}

// Start 3 services, the second fails after run
func TestErrorOnShutdown(t *testing.T) {
	c := service.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	s2 := &testService{
		Name:          "s2",
		ErrorAfterRun: fmt.Errorf("service failed after run"),
	}
	c.Register(s2)

	s3 := &testService{
		Name: "s3",
	}
	c.Register(s3)

	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()
	err := c.StartAll(runCtx)
	require.NoError(t, err)

	// Stop all services, s2 will return an error
	c.StopAll()
	c.WaitAllStopped(context.Background())

	require.Len(t, c.ServiceErrors(), 1)
	errs := c.ServiceErrors()

	assert.NotNil(t, errs["/"+s2.String()])

	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s3)
}

func TestStartAndFailWithError(t *testing.T) {
	c := service.NewContainer()
	c.SetLogger(slog.Default())
	s1 := &testService{
		Name:           "s1",
		ErrorDuringRun: fmt.Errorf("something failed"),
	}
	c.Register(s1)

	ctx := context.Background()
	err := c.StartAll(ctx)
	require.NoError(t, err)
	select {
	case <-ctx.Done():
		t.Fatal("parent context canceled, note that this does no happen!")
	case <-time.After(1 * time.Second):
		assert.Len(t, c.ServiceErrors(), 1)
		//t.Fatal("timeout, expected context to be canceled")
	}
}

// When the context starts to shutdown because of any service error the application want's to get notified
func TestNotifyOnShutdown(t *testing.T) {
	c := service.NewContainer()
	c.SetLogger(slog.Default())
	s1 := &testService{
		Name:           "s1",
		ErrorDuringRun: fmt.Errorf("something failed"),
	}
	c.Register(s1)
	s2 := &testService{
		Name:           "s2",
		ErrorDuringRun: fmt.Errorf("something failed"),
	}
	c.Register(s2)

	s3 := &testService{
		Name: "s3-no-error",
	}
	c.Register(s3)

	ctx, cancelCtx := context.WithCancel(context.Background())
	// s1 fails immediately, StartAll might abort before s2 and s3 are started
	_ = c.StartAll(ctx)

	var shutdownCalls atomic.Int32
	c.OnShutdown(func() {
		shutdownCalls.Add(1)
		cancelCtx()
	})

	ctxIsDone := false
	select {
	case <-ctx.Done():
		ctxIsDone = true
	case <-time.After(1 * time.Second):
		t.Fatal("timeout, expected context to be canceled")
	}

	waitStopped(c, 100*time.Millisecond)
	assert.Equal(t, int32(1), shutdownCalls.Load())
	assert.True(t, ctxIsDone)
	assert.NotEmpty(t, c.ServiceErrors())
}
