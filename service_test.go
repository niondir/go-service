package services_test

import (
	"context"
	"fmt"
	"github.com/niondir/go-services"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

var _ services.Initer = &testService{}
var _ services.Runner = &testService{}
var _ fmt.Stringer = testService{}

// testService is a service that tracks it's state to be checked in tests
type testService struct {
	Name        string
	initialized bool
	started     bool
	running     bool
	stopped     bool
	err         error
}

func (t testService) String() string {
	return fmt.Sprintf("testService.%s", t.Name)
}

func (t *testService) Init(ctx context.Context) error {
	if t.initialized {
		return fmt.Errorf("service %s was already initialized", t.Name)
	}
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

	<-ctx.Done()
	t.running = false

	if t.stopped {
		return fmt.Errorf("service %s was already stopped", t.Name)
	}
	t.stopped = true
	return nil
}

// TODO: Test with outer context that we close to stop everything

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

	c.StopAll(context.Background())

	assert.True(t, s1.initialized)
	assert.True(t, s1.started)
	assert.True(t, s1.stopped)
	assert.False(t, s1.running)
	assert.NoError(t, s1.err)

	assert.True(t, s2.initialized)
	assert.True(t, s2.started)
	assert.True(t, s2.stopped)
	assert.False(t, s2.running)
	assert.NoError(t, s2.err)

}

func TestStartupFail(t *testing.T) {

	srv1Stopped := false
	services.New("srv1").Init(func(ctx context.Context) error {
		return nil
	}).Run(func(ctx context.Context) error {
		<-ctx.Done()
		srv1Stopped = true
		return nil
	}).RegisterDefault()

	srv2Stopped := false
	services.New("srv2").Init(func(ctx context.Context) error {
		return fmt.Errorf("failed")
	}).Run(func(ctx context.Context) error {
		<-ctx.Done()
		srv2Stopped = true
		return nil
	}).RegisterDefault()

	srv3Stopped := false
	services.New("srv3").Init(func(ctx context.Context) error {
		return nil
	}).Run(func(ctx context.Context) error {
		<-ctx.Done()
		srv3Stopped = true
		return nil
	}).RegisterDefault()

	ctx, stop := context.WithCancel(context.Background())

	err := services.Default().StartAll(ctx)
	assert.EqualError(t, err, "failed to init service srv2: failed")
	stop()

	services.Default().WaitAllStopped(context.Background())

	// srv1 was initialized already and must be stopped
	assert.True(t, srv1Stopped)
	// srv2 and srv3 never did run
	assert.False(t, srv2Stopped)
	assert.False(t, srv3Stopped)
}
