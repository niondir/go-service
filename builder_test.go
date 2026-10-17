package service_test

import (
	"context"
	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestServiceBuilder(t *testing.T) {
	c := service.NewContainer()

	initialized := false
	run := false
	stopped := false

	service.New("My Service").
		Init(func(ctx context.Context) error {
			initialized = true
			return nil
		}).
		Run(func(ctx context.Context) error {
			run = true
			// Implement your service here. Try to keep it running, only return fatal errors.
			<-ctx.Done()
			// Gracefully shut down your service here
			stopped = true
			return nil
		}).
		Register(c)

	ctx := context.Background()
	err := c.StartAll(ctx)
	require.NoError(t, err)
	c.StopAll()
	c.WaitAllStopped(ctx)

	assert.Len(t, c.ServiceErrors(), 0)
	assert.True(t, initialized)
	assert.True(t, run)
	assert.True(t, stopped)
}

func TestCtx(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	_, cancel := context.WithCancel(parent)
	cancel()

	<-parent.Done()
	assert.Equal(t, context.DeadlineExceeded, parent.Err())
}
