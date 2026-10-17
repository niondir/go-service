// Package services defines interfaces and methods to run background services in golang applications.
//
// A Service is a somewhat independently running piece of code that runs in it's own go-routine
// it's initialized at some point and stopped later. Think of it as a deamon within the application.
//
// All services are registered during init() or in main() and initialized all together by calling Container.StartAll()
// Services that implement the Initer interface, will run initial Init() code
// All services have to implement the Runner interface. Run() is blocking and only returns when the service stops working.
//
// All services inside one container are started and stopped together. If one service fails, all are stopped.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/pprof"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type RunFunc func(ctx context.Context) error
type InitFunc func(ctx context.Context) error

type genericService struct {
	name string
	init InitFunc
	run  RunFunc
}

func (sr *genericService) Init(ctx context.Context) error {
	return sr.init(ctx)
}

func (sr *genericService) Run(ctx context.Context) error {
	return sr.run(ctx)
}

func (sr *genericService) String() string {
	return sr.name
}

// runOnlyService is a service without init code. It does not implement Initer.
type runOnlyService struct {
	name string
	run  RunFunc
}

func (sr *runOnlyService) Run(ctx context.Context) error {
	return sr.run(ctx)
}

func (sr *runOnlyService) String() string {
	return sr.name
}

// newGenericService returns a service that only implements Initer if initFn is set
func newGenericService(name string, initFn InitFunc, runFn RunFunc) Runner {
	if initFn == nil {
		return &runOnlyService{name, runFn}
	}
	return &genericService{name, initFn, runFn}
}

type serviceInfo struct {
	name    string
	service Runner
}

// hasInit reports whether the service has init code
func hasInit(s *serviceInfo) bool {
	_, ok := s.service.(Initer)
	return ok
}

type runContext struct {
	service *serviceInfo
	// done is closed when Run returned. Only valid if started is true.
	done chan struct{}

	mu sync.Mutex
	// started is true once the Run goroutine was launched
	started bool
	running bool
	err     error
}

func newRunContext(s *serviceInfo) *runContext {
	return &runContext{
		service: s,
		done:    make(chan struct{}),
	}
}

func (rc *runContext) isRunning() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.running
}

func (rc *runContext) getErr() error {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.err
}

func (rc *runContext) isStarted() bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.started
}

// wait blocks until Run returned. Returns immediately if Run was never started.
func (rc *runContext) wait() {
	if !rc.isStarted() {
		return
	}
	<-rc.done
}

// Container with all services
// The Container handles the following lifecycle:
// - Register all services
// - Start all services
// - Stop all services
// If a single service fails during init or run, all services inside the container are stopped.
type Container struct {
	// name is used to optionally identify the container
	name string
	// Context passed to StartAll
	parentCtx context.Context
	// Context in which all services are running
	runCtx context.Context
	// Cancel method of the runCtx, when called all services should stop. The cause is the *StopError.
	runCtxCancel context.CancelCauseFunc
	services     []*serviceInfo
	log          *slog.Logger

	stoppingLogOnce sync.Once
	// stoppingLogged is closed after "Stopping all services" was logged
	stoppingLogged chan struct{}
	stoppedLogOnce sync.Once
	// startMu serializes starting a service with the begin of the shutdown,
	// so no service is started after "Stopping all services" was logged
	startMu sync.Mutex

	// mu guards the fields below
	mu                sync.Mutex
	runContexts       map[string]*runContext
	shutdownCallbacks []func()
	shutdownStarted   bool
	stopCause         *StopError
}

type Option func(c *Container)

func NewContainer(opts ...Option) *Container {
	nopLogger := slog.New(NopHandler{})
	c := &Container{
		services:       make([]*serviceInfo, 0),
		runContexts:    map[string]*runContext{},
		log:            nopLogger,
		stoppingLogged: make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func WithName(name string) Option {
	return func(c *Container) {
		c.name = name
	}
}

var defaultContainer *Container

func Default() *Container {
	if defaultContainer == nil {
		defaultContainer = NewContainer()
		defaultContainer.name = "default"
	}
	return defaultContainer
}

func (c *Container) Name() string {
	return c.name
}

func (c *Container) SetLogger(logger *slog.Logger) {
	c.log = logger
}

// logger returns the logger with the container name attached
func (c *Container) logger() *slog.Logger {
	return c.log.With("container", c.name)
}

// Register adds a service to the list of services to be initialized
func (c *Container) Register(service Runner) {
	name := fmt.Sprintf("%T", service)
	if s, ok := service.(fmt.Stringer); ok {
		name = s.String()
	}

	for _, s := range c.services {
		if s.name == name {
			panic(fmt.Sprintf("Service '%s' already registered in container %s", name, c.name))
		}
	}

	c.services = append(c.services, &serviceInfo{
		name:    name,
		service: service,
	})
	c.logger().Info("Registered service", "service", name)
}

func (c *Container) getRunContext(name string) (*runContext, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc, ok := c.runContexts[name]
	return rc, ok
}

// runContextList returns a snapshot of all run contexts
func (c *Container) runContextList() []*runContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	rcs := make([]*runContext, 0, len(c.runContexts))
	for _, rc := range c.runContexts {
		rcs = append(rcs, rc)
	}
	return rcs
}

func (c *Container) initOne(ctx context.Context, s *serviceInfo) error {
	c.onInit(s)
	runner := newRunContext(s)
	c.mu.Lock()
	if _, ok := c.runContexts[s.name]; ok {
		c.mu.Unlock()
		return fmt.Errorf("service '%s' already started in container '%s'", s.name, c.name)
	}
	c.runContexts[s.name] = runner
	c.mu.Unlock()

	// Execute initialization code if any
	initer, ok := s.service.(Initer)
	if !ok {
		return nil
	}

	logger := c.logger().With("service", s.name)
	logger.Info("Initializing service")
	err := callSafe(func() error { return initer.Init(ctx) })
	if err != nil {
		return err
	}
	logger.Info("Initialized service")
	return nil
}

func (c *Container) runOne(ctx context.Context, s *serviceInfo) error {
	c.onRun(s)
	runner, ok := c.getRunContext(s.name)
	if !ok {
		return fmt.Errorf("service '%s' not initialized in container '%s'", s.name, c.name)
	}
	runner.mu.Lock()
	if runner.started {
		runner.mu.Unlock()
		return fmt.Errorf("service '%s' already running in container '%s'", s.name, c.name)
	}
	runner.started = true
	runner.running = true
	runner.mu.Unlock()

	// Log before launching the goroutine, so the log order matches the start order
	c.logger().Info("Starting service", "service", s.name)
	// Execute the actual run method in background
	go func() {
		var runErr error
		pprof.Do(ctx, pprof.Labels("go_service_name", s.name, "go_service_container", c.name), func(ctx context.Context) {
			runErr = callSafe(func() error { return s.service.Run(ctx) })
		})
		c.logRunResult(s.name, runErr)

		runner.mu.Lock()
		runner.err = runErr
		runner.running = false
		runner.mu.Unlock()
		close(runner.done)

		if runErr != nil {
			c.shutdown()
		}
	}()

	return nil
}

// logRunResult logs why a service stopped and records the stop cause if this service triggered the stop.
func (c *Container) logRunResult(name string, runErr error) {
	logger := c.logger().With("service", name)

	if runErr == nil {
		if c.isStopping() {
			c.waitStoppingLogged()
			logger.Info("Service stopped")
		} else {
			logger.Warn("Service stopped unexpectedly, Run() returned nil before the container was stopped. Other services keep running.")
		}
		return
	}

	var panicErr *PanicError
	isPanic := errors.As(runErr, &panicErr)
	reason := StopReasonServiceFailed
	if isPanic {
		reason = StopReasonPanic
	}

	if c.claimStop(&StopError{Reason: reason, Container: c.name, Service: name, Err: runErr}) {
		// This service is the root cause of the shutdown. It is logged before "Stopping all services",
		// all other services wait for that log line.
		if isPanic {
			logger.Error("Service panicked", "error", runErr, "stack", string(panicErr.Stack))
		} else {
			logger.Error("Service failed", "error", runErr)
		}
		return
	}

	// The container was already stopping, this error is most likely a consequence of the shutdown
	c.waitStoppingLogged()
	if isContextErr(runErr) {
		logger.Info("Service stopped", "error", runErr)
		return
	}
	attrs := []any{"error", runErr, "stop_cause", c.StopCause()}
	if isPanic {
		attrs = append(attrs, "stack", string(panicErr.Stack))
	}
	logger.Warn("Service stopped with error during shutdown", attrs...)
}

// parentStopError returns the stop error for a done parent context, nil if the parent is not done.
func (c *Container) parentStopError() *StopError {
	if c.parentCtx == nil || c.parentCtx.Err() == nil {
		return nil
	}
	return &StopError{Reason: StopReasonParentContext, Container: c.name, Err: context.Cause(c.parentCtx)}
}

// claimStop records the cause of the shutdown. Only the first call wins and returns true.
// A done parent context always wins against later causes, since it happened first.
// Calling it with nil only records a done parent context.
func (c *Container) claimStop(cause *StopError) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopCause == nil {
		c.stopCause = c.parentStopError()
	}
	if c.stopCause != nil || cause == nil {
		return false
	}
	c.stopCause = cause
	return true
}

// isStopping reports whether the container is shutting down, it also records a done parent context as stop cause.
func (c *Container) isStopping() bool {
	c.claimStop(nil)
	return c.StopCause() != nil || (c.runCtx != nil && c.runCtx.Err() != nil)
}

// waitStoppingLogged blocks until "Stopping all services" was logged, to keep the log order.
func (c *Container) waitStoppingLogged() {
	select {
	case <-c.stoppingLogged:
	case <-time.After(time.Second):
		// Safety net, should not happen since every claimed stop triggers a shutdown
	}
}

// shutdown logs the stop cause, cancels all services and then runs the shutdown callbacks.
// Callbacks run after the services were canceled, so they can wait for the services to stop.
func (c *Container) shutdown() {
	c.logStopping()
	c.runCtxCancel(c.StopCause())
	c.runShutdownCallbacks()
}

// logStopping logs the stop cause exactly once.
func (c *Container) logStopping() {
	c.stoppingLogOnce.Do(func() {
		defer close(c.stoppingLogged)
		cause := c.getStopCause()
		if cause == nil {
			panic("go-service: shutdown without stop cause")
		}
		attrs := []any{"reason", string(cause.Reason)}
		if cause.Service != "" {
			attrs = append(attrs, "service", cause.Service)
		}
		if cause.Err != nil {
			attrs = append(attrs, "error", cause.Err)
		}
		c.startMu.Lock()
		defer c.startMu.Unlock()
		switch cause.Reason {
		case StopReasonStopAll, StopReasonParentContext:
			c.logger().Info("Stopping all services", attrs...)
		default:
			c.logger().Warn("Stopping all services", attrs...)
		}
	})
}

// runShutdownCallbacks runs the shutdown callbacks once. Later calls, e.g. StopAll() from inside a callback,
// return immediately instead of waiting for the callbacks.
func (c *Container) runShutdownCallbacks() {
	c.mu.Lock()
	if c.shutdownStarted {
		c.mu.Unlock()
		return
	}
	c.shutdownStarted = true
	callbacks := c.shutdownCallbacks
	c.mu.Unlock()
	c.onStopAll(callbacks)
}

// watchParent triggers the shutdown when the run context is done, e.g. because the parent context was canceled.
func (c *Container) watchParent() {
	<-c.runCtx.Done()
	c.claimStop(nil)
	c.shutdown()
}

// StartAll starts all services inside the container
// the function does not block, services are started in background
func (c *Container) StartAll(ctx context.Context) error {
	if c.runCtx != nil {
		panic("Container.StartAll can only be called once")
	}
	c.parentCtx = ctx
	c.runCtx, c.runCtxCancel = context.WithCancelCause(ctx)
	go c.watchParent()

	// Iterate over all services to initialize them
	for i := range c.services {
		s := c.services[i]
		// TODO: Should we allow services to optionally initialize in parallel? Then we might get multiple errors returned
		err := c.initOne(c.runCtx, s)
		if err != nil {
			c.handleInitError(s, i, err)
			return fmt.Errorf("failed to init service %s: %w", s.name, err)
		}
	}

	// Iterate over all services to run them
	for i := range c.services {
		s := c.services[i]
		c.startMu.Lock()
		if c.isStopping() {
			c.startMu.Unlock()
			cause := c.StopCause()
			// A service already failed or the parent context is done, do not start any further services
			c.logger().Warn("Container is stopping, skip starting remaining services",
				"skipped", serviceNames(c.services[i:]), "stop_cause", cause)
			c.shutdown()
			return fmt.Errorf("start aborted: %w", cause)
		}
		err := c.runOne(c.runCtx, s)
		c.startMu.Unlock()
		if err != nil {
			c.claimStop(&StopError{Reason: StopReasonStartFailed, Container: c.name, Service: s.name, Err: err})
			c.logger().Error("Failed to start service", "service", s.name, "error", err)
			c.shutdown()
			return err
		}
	}
	c.logger().Info("All services started", "count", len(c.services))

	return nil
}

func (c *Container) handleInitError(s *serviceInfo, idx int, err error) {
	logger := c.logger().With("service", s.name)
	var panicErr *PanicError
	if errors.As(err, &panicErr) {
		c.claimStop(&StopError{Reason: StopReasonPanic, Container: c.name, Service: s.name, Err: err})
		logger.Error("Service panicked during init", "error", err, "stack", string(panicErr.Stack))
	} else {
		c.claimStop(&StopError{Reason: StopReasonInitFailed, Container: c.name, Service: s.name, Err: err})
		logger.Error("Failed to initialize service", "error", err)
	}
	if len(c.services) > 1 {
		others := filterServices(c.services, func(o *serviceInfo) bool { return o != s })
		c.logger().Warn("Services not started because of init failure",
			"initialized", serviceNames(filterServices(c.services[:idx], hasInit)),
			"not_started", serviceNames(others))
	}
	c.shutdown()
}

func filterServices(services []*serviceInfo, keep func(*serviceInfo) bool) []*serviceInfo {
	res := make([]*serviceInfo, 0, len(services))
	for _, s := range services {
		if keep(s) {
			res = append(res, s)
		}
	}
	return res
}

func serviceNames(services []*serviceInfo) []string {
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.name)
	}
	return names
}

// WaitAllRunningTimeout blocks until all services are running or the context is canceled
// NOTE: We want to introduce a version with context instead of the duration. But that needs some refactoring in current client
// thus this will be deprecated in future and has the "Timeout" stated in the name
func (c *Container) WaitAllRunningTimeout(timeout time.Duration) bool {
	wg := sync.WaitGroup{}
	allReady := atomic.Bool{}
	// Assume all are ready, until one is not
	allReady.Store(true)
	for _, runCtx := range c.runningServices() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if waiter, ok := runCtx.service.service.(ReadyWaiter); ok {
				ready := waiter.WaitReady(timeout)
				if !ready {
					allReady.Store(false)
				}
			}
		}()
	}
	wg.Wait()
	return allReady.Load()
}

// IsRunning returns true if StartAll was called on the container.
// It stays true after the services stopped, use RunningCount() to check for running services.
func (c *Container) IsRunning() bool {
	return c.runCtx != nil
}

// StopAll gracefully stops all services.
// It is safe to call StopAll multiple times and from inside an OnShutdown callback.
func (c *Container) StopAll() {
	if c.runCtxCancel == nil {
		panic("call Container.StartAll() before StopAll()")
	}
	c.claimStop(&StopError{Reason: StopReasonStopAll, Container: c.name})
	c.shutdown()
}

func (c *Container) getStopCause() *StopError {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopCause
}

// StopCause returns why the container is stopping, nil if it was not stopped yet.
// The returned error is a *StopError and unwraps to the error of the service that caused the stop.
func (c *Container) StopCause() error {
	if cause := c.getStopCause(); cause != nil {
		return cause
	}
	return nil
}

func (c *Container) runningServices() []*runContext {
	rcs := make([]*runContext, 0)
	for _, rc := range c.runContextList() {
		if rc.isRunning() {
			rcs = append(rcs, rc)
		}
	}
	return rcs
}

func (c *Container) RunningCount() int {
	return len(c.runningServices())
}

// RunningServiceNames returns the names of all services whose Run() did not return yet.
func (c *Container) RunningServiceNames() []string {
	names := make([]string, 0)
	for _, rc := range c.runningServices() {
		names = append(names, rc.service.name)
	}
	sort.Strings(names)
	return names
}

func (c *Container) ServiceNames() []string {
	var names []string

	for _, rc := range c.runContextList() {
		names = append(names, rc.service.name)
	}

	return names
}

// WaitAllStopped blocks until all services are stopped or ctx is done.
// After ctx is done, services might still run. Call Container.StopAll() to stop them.
// If ctx is done first, the names of the services that are still running are logged, see also RunningServiceNames().
func (c *Container) WaitAllStopped(ctx context.Context) {
	if c.runCtxCancel == nil {
		panic("call Container.StartAll() before WaitAllStopped()")
	}

	rcs := c.runContextList()
	wg := sync.WaitGroup{}
	wg.Add(len(rcs))
	for _, rc := range rcs {
		go func() {
			rc.wait()
			c.onStopped(rc)
			wg.Done()
		}()
	}

	allStopped := make(chan struct{})
	// wait till all services are stopped
	go func() {
		wg.Wait()
		close(allStopped)
	}()

	select {
	case <-allStopped:
		c.stoppedLogOnce.Do(func() { c.logStopped(rcs) })
	case <-ctx.Done():
		c.logger().Warn("Timeout waiting for services to stop",
			"cause", context.Cause(ctx),
			"running", c.RunningServiceNames(),
			"stop_cause", c.StopCause())
	}
}

// logStopped logs the summary after all services stopped.
// count: services whose Run() was called, failed: services that caused the stop or returned an error other than a context error
func (c *Container) logStopped(rcs []*runContext) {
	cause := c.getStopCause()
	failed := map[string]bool{}
	started := 0
	for _, rc := range rcs {
		rc.mu.Lock()
		if rc.started {
			started++
		}
		if rc.err != nil && !isContextErr(rc.err) {
			failed[rc.service.name] = true
		}
		rc.mu.Unlock()
	}
	if cause != nil && cause.Service != "" {
		failed[cause.Service] = true
	}
	attrs := []any{"count", started, "failed", len(failed)}
	if cause != nil {
		attrs = append(attrs, "stop_cause", cause)
	}
	c.logger().Info("All services stopped", attrs...)
}

// ServiceErrors returns all errors occurred in services
// The key is "<container name>/<service name>"
func (c *Container) ServiceErrors() map[string]error {
	errs := map[string]error{}
	for _, rc := range c.runContextList() {
		if err := rc.getErr(); err != nil {
			errs[fmt.Sprintf("%s/%s", c.name, rc.service.name)] = err
		}
	}
	return errs
}

// onStopAll calls the shutdown callbacks
// This method is only called once per container
func (c *Container) onStopAll(callbacks []func()) {
	for _, f := range callbacks {
		f()
	}
}

// onInit is called before a service Init method is called
func (c *Container) onInit(s *serviceInfo) {

}

// onRun is called before a service Run method is called
func (c *Container) onRun(s *serviceInfo) {

}

// onStopped is called after a service was stopped
func (c *Container) onStopped(rc *runContext) {

}

// OnShutdown registers a callback that is called once when the container starts to stop.
// The services are already canceled when the callback runs, so it can wait for them via WaitAllStopped.
// If the container is already shutting down, f is called immediately.
func (c *Container) OnShutdown(f func()) {
	c.mu.Lock()
	if c.shutdownStarted {
		c.mu.Unlock()
		f()
		return
	}
	c.shutdownCallbacks = append(c.shutdownCallbacks, f)
	c.mu.Unlock()
}
