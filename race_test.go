package service_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/niondir/go-service"
	"github.com/stretchr/testify/assert"
)

// Status methods must be safe to call while services start and stop concurrently.
func TestStatusMethodsConcurrentWithServices(t *testing.T) {
	c := service.NewContainer()
	for i := 0; i < 10; i++ {
		i := i
		service.New(fmt.Sprintf("s%d", i)).Run(func(ctx context.Context) error {
			if i%3 == 0 {
				return fmt.Errorf("s%d failed", i)
			}
			<-ctx.Done()
			return nil
		}).Register(c)
	}
	// StartAll may be aborted, since services fail while others are still started
	_ = c.StartAll(context.Background())

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = c.RunningCount()
				_ = c.ServiceErrors()
				_ = c.ServiceNames()
			}
		}()
	}
	waitStopped(c, time.Second)
	wg.Wait()
	assert.Equal(t, 0, c.RunningCount())
	// Services registered after the first failing one might be skipped, see StartAll
	assert.NotEmpty(t, c.ServiceErrors())
	assert.Error(t, c.StopCause())
}
