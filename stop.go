package service

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

// StopReason describes what triggered the shutdown of a Container.
type StopReason string

const (
	// StopReasonServiceFailed means a service returned an error from Run() before the container was stopped.
	StopReasonServiceFailed StopReason = "service failed"
	// StopReasonInitFailed means a service returned an error from Init().
	StopReasonInitFailed StopReason = "service init failed"
	// StopReasonPanic means a service panicked in Init() or Run().
	StopReasonPanic StopReason = "service panicked"
	// StopReasonStartFailed means the container could not start a service.
	StopReasonStartFailed StopReason = "service start failed"
	// StopReasonStopAll means Container.StopAll() was called.
	StopReasonStopAll StopReason = "StopAll called"
	// StopReasonParentContext means the context passed to Container.StartAll() was done.
	StopReasonParentContext StopReason = "parent context done"
)

// StopError describes why a Container was stopped. See Container.StopCause().
type StopError struct {
	Reason StopReason
	// Container is the name of the container that stopped
	Container string
	// Service is the name of the service that triggered the stop, empty if no service was involved.
	Service string
	// Err is the underlying error, nil for StopReasonStopAll.
	Err error
}

func (e *StopError) Error() string {
	msg := string(e.Reason)
	if e.Service != "" {
		msg = fmt.Sprintf("%s: %s", msg, e.Service)
	}
	if e.Err != nil {
		msg = fmt.Sprintf("%s: %v", msg, e.Err)
	}
	return msg
}

func (e *StopError) Unwrap() error {
	return e.Err
}

// PanicError is returned (and stored as service error) when a service panics in Init() or Run().
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v", e.Value)
}

// callSafe calls f and converts a panic into a *PanicError.
func callSafe(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return f()
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
