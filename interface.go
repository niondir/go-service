package services

import "context"

// Runner defines the interface to execute service related code in background

type Runner interface {
	// Run is executed inside its own go-routine and must not return until the service stops.
	// Run must return after <-ctx.Done() and shutdown gracefully
	// When an error is returned, all services inside the container will be stopped
	Run(ctx context.Context) error
}

// Initer can be optionally implemented for services that need to run initial startup code
// All init methods of registered services are executed sequentially
// When a starter returns an error, no further services are executed and the application shuts down
type Initer interface {
	Init(ctx context.Context) error
}

type Waiter interface {
	Wait()
}
