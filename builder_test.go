package services_test

import (
	"context"
	"github.com/niondir/go-services"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestServiceBuilder(t *testing.T) {
	c := services.NewContainer()

	initialized := false
	run := false
	stopped := false

	services.New("My Service").
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

	err := c.StartAll(context.Background())
	require.NoError(t, err)
	c.StopAll()
	c.WaitAllStopped()

	assert.Len(t, c.ServiceErrors(), 0)
	assert.True(t, initialized)
	assert.True(t, run)
	assert.True(t, stopped)
}
