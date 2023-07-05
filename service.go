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
	"fmt"
	"golang.org/x/exp/slog"
	"sync"
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
	if sr.init == nil {
		return nil
	}
	return sr.init(ctx)
}
func (sr *genericService) Run(ctx context.Context) error {
	return sr.run(ctx)
}

func (sr *genericService) String() string {
	return sr.name
}

type runContext struct {
	service *serviceInfo
	running bool
	done    chan error
	err     error
}

type serviceInfo struct {
	name    string
	service Runner
}

func (rc *runContext) wait() {
	if !rc.running {
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
	// Context in which all services are running
	runCtx context.Context
	// Cancel method of the runCtx, when called all services should stop
	runCtxCancel context.CancelFunc
	services     []*serviceInfo
	runContexts  map[string]*runContext
	log          *slog.Logger
}

func NewContainer() *Container {
	nopLogger := slog.New(NopHandler{})
	return &Container{
		services:    make([]*serviceInfo, 0),
		runContexts: map[string]*runContext{},
		log:         nopLogger,
	}
}

var defaultContainer *Container

func Default() *Container {
	if defaultContainer == nil {
		defaultContainer = NewContainer()
	}
	return defaultContainer
}

func (c *Container) SetLogger(logger *slog.Logger) {
	c.log = logger
}

// Register adds a service to the list of services to be initialized
func (c *Container) Register(service Runner) {
	name := fmt.Sprintf("%T", service)
	if s, ok := service.(fmt.Stringer); ok {
		name = s.String()
	}

	for _, s := range c.services {
		if s.name == name {
			panic(fmt.Sprintf("Service '%s' already registered", name))
		}
	}

	c.services = append(c.services, &serviceInfo{
		name:    name,
		service: service,
	})
	c.log.Info("Registered service", "name", name)
}

func newRunContext(s *serviceInfo) *runContext {
	return &runContext{
		service: s,
		done:    make(chan error, 1),
	}
}

func (c *Container) initOne(ctx context.Context, s *serviceInfo) error {
	c.onInit(s)
	runner := newRunContext(s)
	if _, ok := c.runContexts[s.name]; ok {
		return fmt.Errorf("service '%s' already started", s.name)
	}

	c.runContexts[s.name] = runner

	// Execute initialization code if any
	if initer, ok := s.service.(Initer); ok {
		c.log.Info("Initializing service", "name", s.name)
		err := initer.Init(ctx)
		if err != nil {
			go func() {
				// Let the runner stop immediately
				// The error is nil, since it is the "Run()" error
				runner.done <- nil
			}()
			c.log.Debug("Failed to initialize service", "name", s.name, "error", err)
			return fmt.Errorf("failed to init service %s: %w", s.name, err)
		}
		c.log.Info("Initialized service", "name", s.name)
	}

	return nil
}

func (c *Container) runOne(ctx context.Context, s *serviceInfo) error {
	c.onRun(s)
	runner, ok := c.runContexts[s.name]
	if !ok {
		return fmt.Errorf("service '%s' not initialized", s.name)
	}
	if runner.running {
		return fmt.Errorf("service '%s' already running", s.name)
	}

	// Execute the actual run method in background
	runner.running = true
	go func() {
		logger := c.log.With("name", s.name)
		logger.Info("Starting service")
		runErr := s.service.Run(ctx)
		if runErr != nil {
			logger.Error("Service stopped with error", "error", runErr)
		} else {
			logger.Info("Service stopped")
		}
		runner.err = runErr
		runner.running = false
		close(runner.done)
		if runErr != nil {
			c.onStopAll()
			c.StopAll()
		}
	}()

	return nil
}

// StartAll starts all services inside the container
// the function does not block, services are started in background
func (c *Container) StartAll(ctx context.Context) error {
	if c.runCtx != nil {
		panic("Container.StartAll can only be called once")
	}
	c.runCtx, c.runCtxCancel = context.WithCancel(ctx)

	// Iterate over all services to initialize them
	for i := range c.services {
		s := c.services[i]
		// TODO: Should we allow services to optionally initialize in parallel? Then we might get multiple errors returned
		err := c.initOne(c.runCtx, s)
		if err != nil {
			c.StopAll()
			return err
		}
	}

	// Iterate over all services to run them
	for i := range c.services {
		s := c.services[i]
		err := c.runOne(c.runCtx, s)
		if err != nil {
			c.StopAll()
			return err
		}
	}

	return nil
}

// StopAll gracefully stops all services.
// If you need a timeout, passe a context with Timeout or Deadline
func (c *Container) StopAll() {
	if c.runCtxCancel == nil {
		panic("call Container.StartAll() before StopAll()")
	}
	c.runCtxCancel()
}

func (c *Container) runningServices() []*runContext {
	rcs := make([]*runContext, 0)
	for i := range c.runContexts {
		rc := c.runContexts[i]
		if rc.running {
			rcs = append(rcs, rc)
		}
	}
	return rcs
}

func (c *Container) RunningCount() int {
	cnt := 0
	for _, rc := range c.runContexts {
		if rc.running {
			cnt++
		}
	}
	return cnt
}

func (c *Container) ServiceNames() []string {
	var names []string

	for _, rc := range c.runContexts {
		names = append(names, rc.service.name)
	}

	return names
}

func (c *Container) WaitAllStopped() {
	c.WaitAllStoppedTimeout(0)
}

// WaitAllStoppedTimeout blocks until all services are stopped or timeout is exceeded
// calling with timout of 0 will wait forever - better use WaitAllStopped() then.
// After the timeout is reached, services might still run. Call Container.StopAll() to stop them.
func (c *Container) WaitAllStoppedTimeout(timeout time.Duration) {
	if c.runCtxCancel == nil {
		panic("call Container.StartAll() before WaitAllStopped()")
	}

	var ctx context.Context
	var cancel context.CancelFunc

	if timeout != 0 {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)

	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	wg := sync.WaitGroup{}
	wg.Add(len(c.runContexts))
	for k := range c.runContexts {
		rc := c.runContexts[k]
		go func() {
			rc.wait()
			c.onStopped(rc)
			wg.Done()
		}()
	}

	// wait till all services are stopped
	go func() {
		wg.Wait()
		cancel()
	}()

	<-ctx.Done()
}

// ServiceErrors returns all errors occurred in services
func (c *Container) ServiceErrors() map[string]error {
	errs := map[string]error{}
	for _, rc := range c.runContexts {
		if rc.err != nil {
			errs[rc.service.name] = rc.err
		}
	}
	return errs
}

// onStopAll is called when all services get stopped
func (c *Container) onStopAll() {

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
