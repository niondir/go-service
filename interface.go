package services

import "context"

// Runner defines the interface to execute service related code in background

type Runner interface {
	// Run is executed inside it's own go-routine and must not return until the service stops.
	// Run should only return after ctx.Done() or when a non recoverable error occurs.
	// Returning an error means the service did fail. On ctx.Done() the service should shutdown gracefully.
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
