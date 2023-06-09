package service_test

import (
	"context"
	"github.com/niondir/go-service"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestWithRunFunc(t *testing.T) {
	c := service.NewContainer()
	started := false
	stopped := false

	anon := func(ctx context.Context) error {
		started = true
		t.Logf("service started")
		<-ctx.Done()
		stopped = true
		t.Logf("service stopped")
		return nil
	}

	c.Register(service.WithRunFunc(anon))

	ctx, cancel := context.WithCancel(context.Background())
	err := c.StartAll(ctx)

	t.Logf("Services: %v", c.ServiceNames())
	require.NoError(t, err)
	cancel()
	c.WaitAllStoppedTimeout(time.Second)
	require.True(t, started)
	require.True(t, stopped)
}

func TestWithFunc(t *testing.T) {
	initialized := false
	started := false
	stopped := false

	c := service.NewContainer()
	init := func(ctx context.Context) error {
		initialized = true
		t.Logf("service initialized")
		return nil
	}
	run := func(ctx context.Context) error {
		started = true
		t.Logf("service started")
		<-ctx.Done()
		stopped = true
		t.Logf("service stopped")
		return nil
	}

	c.Register(service.WithFunc(init, run))

	ctx, cancel := context.WithCancel(context.Background())
	err := c.StartAll(ctx)

	t.Logf("Services: %v", c.ServiceNames())
	require.NoError(t, err)
	cancel()
	c.WaitAllStoppedTimeout(time.Second)
	require.True(t, initialized)
	require.True(t, started)
	require.True(t, stopped)
}
