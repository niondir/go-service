package services_test

import (
	"context"
	"fmt"
	"github.com/niondir/go-services"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

var _ services.Initer = &testService{}
var _ services.Runner = &testService{}
var _ fmt.Stringer = testService{}

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
	initialized    bool
	started        bool
	running        bool
	stopped        bool
	err            error
	startedCh      chan struct{}
}

func (t testService) String() string {
	return fmt.Sprintf("testService.%s", t.Name)
}

func (t *testService) Init(ctx context.Context) error {
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
	if t.running {
		return fmt.Errorf("service %s already running", t.Name)
	}
	t.running = true
	if t.started {
		return fmt.Errorf("service %s was already started", t.Name)
	}
	t.started = true
	if t.startedCh != nil {
		close(t.startedCh)
	}

	if t.ErrorDuringRun != nil {
		t.running = false
		t.stopped = true
		return t.ErrorDuringRun
	}

	if !t.SkipWaitForCtx {
		<-ctx.Done()
	}
	t.running = false

	if t.stopped {
		return fmt.Errorf("service %s was already stopped", t.Name)
	}
	t.stopped = true

	return t.ErrorAfterRun
}

func assertServiceStartedAndStopped(t *testing.T, s *testService) {
	t.Helper()
	assert.True(t, s.initialized, "initialized")
	assert.True(t, s.started, "started")
	assert.True(t, s.stopped, "stopped")
	assert.False(t, s.running, "running")
	assert.NoError(t, s.err, "err")
}

func assertServiceStillRunning(t *testing.T, s *testService) {
	t.Helper()
	assert.True(t, s.initialized)
	assert.True(t, s.started)
	assert.False(t, s.stopped, "Stopped")
	assert.True(t, s.running, "Still Running")
	assert.NoError(t, s.err)
}

func assertServiceOnlyInitialized(t *testing.T, s *testService) {
	t.Helper()
	assert.True(t, s.initialized)
	assert.False(t, s.started)
	assert.False(t, s.stopped)
	assert.False(t, s.running)
	assert.NoError(t, s.err)
}

func assertServiceNeverStarted(t *testing.T, s *testService) {
	t.Helper()
	assert.False(t, s.initialized)
	assert.False(t, s.started)
	assert.False(t, s.stopped)
	assert.False(t, s.running)
	assert.NoError(t, s.err)
}

func TestStartAndStopWithContext(t *testing.T) {
	c := services.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	ctx, cancelCtx := context.WithCancel(context.Background())
	err := c.StartAll(ctx)
	require.NoError(t, err)

	cancelCtx()
	c.WaitAllStopped()
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
}

func TestStartAndStopWithContext_timeout(t *testing.T) {
	c := services.NewContainer()
	s1 := &testService{
		Name: "s1",
	}
	c.Register(s1)

	err := c.StartAll(context.Background())
	require.NoError(t, err)

	c.WaitAllStoppedTimeout(100 * time.Millisecond)
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStillRunning(t, s1)
}

// Start and Stop multiple services (happy path)
func TestStartAndStop(t *testing.T) {
	c := services.NewContainer()
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
	c.WaitAllStopped()
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
}

// Start 3 services, the second will just return but the other two will keep running
func TestServiceCanReturnWithoutError(t *testing.T) {
	c := services.NewContainer()
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

	// Wait all started
	<-s1.startedCh
	<-s2.startedCh
	<-s3.startedCh
	assertServiceStillRunning(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStillRunning(t, s3)

	c.StopAll()
	c.WaitAllStopped()
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s2)
}

// Start 3 services, the second fails during init, none should run
func TestStopWhenInitFails(t *testing.T) {
	c := services.NewContainer()
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
	c.WaitAllStopped()
	assert.Len(t, c.ServiceErrors(), 0)
	assertServiceOnlyInitialized(t, s1)
	assertServiceNeverStarted(t, s2)
	assertServiceNeverStarted(t, s3)
}

// Start 3 services, the second fails during run
func TestStopWhenRunFails(t *testing.T) {
	c := services.NewContainer()
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
	c.WaitAllStopped()

	require.Len(t, c.ServiceErrors(), 1)
	errs := c.ServiceErrors()

	assert.NotNil(t, errs[s2.String()])

	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s3)
}

// Start 3 services, the second fails after run
func TestErrorOnShutdown(t *testing.T) {
	c := services.NewContainer()
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
	c.WaitAllStopped()

	require.Len(t, c.ServiceErrors(), 1)
	errs := c.ServiceErrors()

	assert.NotNil(t, errs[s2.String()])

	assertServiceStartedAndStopped(t, s1)
	assertServiceStartedAndStopped(t, s2)
	assertServiceStartedAndStopped(t, s3)
}
