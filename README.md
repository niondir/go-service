# go Service

A package to manage background services in go applications.


# Overview

`Services` are organized inside a `Container`. 
All services must be registered first and are started via the container.



# Usage


## Implement your service 

A service is as simple as implementing the `Runner` interface:

```
type Runner interface {
	// Run is executed inside its own go-routine and must not return until the service stops.
	// Run must return after <-ctx.Done() and shutdown gracefully
	// When an error is returned, all services inside the container will be stopped
	Run(ctx context.Context) error
}
```

Service struct boilerplate. Initer is optional (see below).
```
var _ service.Runner = &MyService{}
var _ service.Initer = &MyService{}

type MyService struct {
	// Whatever is needed in context of the service
}

func (s *MyService) Init(ctx context.Context) error {
	return nil
}

func (s *MyService) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		// Optional shutdown logic, e.g. http.Shutdown(shutdownCtx)
	}()
	
	// Usually blocking code like http.ListenAndServe(), else you can also wait for <-ctx.Done()
	// After gracefull shutdown return nil, other services will continue to work
	// If an error is returned all services inside the same container will also be stopped
	
	return nil
}
```

And register them inside a container:

```
	c := service.NewContainer() // or use service.Default()
	c.Register(s1)
```

Service names must be unique inside a single container.

### Register with builder
There is also a builder pattern if you prefer not to implement the interface yourself:

```
	c := service.NewContainer()
	
	service.New("My Service").Run(func(ctx context.Context) error {
		// Implement your service here. Try to keep it running, only return fatal errors.
		<-ctx.Done()
		// Gracefully shut down your service here
		return nil
	}).Register(c)
```

### Register as function
If you just want to register a single function as service you can use the following helper.

```
service.Default().Register(service.WithFunc(init, run))
service.Default().Register(service.WithRunFunc(run))
```

Service names are derived from the function name via reflection.

## Start and Stop your services

After registering all services you can start them all together.

```
	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()
	err := c.StartAll(runCtx)
	// err comes from the initialization (see below)
```

Stop all services, by either calling `c.StopAll()` or `runCtxCancel()`.
All services also stop if any `Run()` function returns an error.

You can actively wait for all services to stop:

```
	// Blocks until all services stopped or ctx is done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.WaitAllStopped(ctx)

	// Why did the container stop? nil if it was not stopped.
	// The error is a *service.StopError with Reason, Service and the underlying Err.
	cause := c.StopCause()

	// All errors returned by Run(), including errors returned during shutdown.
	// Keys are "<container name>/<service name>".
	errs := c.ServiceErrors()

	// After WaitAllStopped returned because ctx was done: which services did not stop yet?
	names := c.RunningServiceNames()
```

Wait until all services are ready (see `ReadyWaiter`) before you serve traffic:

```
	// Returns nil when all running services are ready. Otherwise an error:
	//  - the *service.StopError (see StopCause) if the container stopped, also if a service failed while waiting
	//  - an error wrapping context.Cause(ctx) if ctx is done
	//  - an error wrapping service.ErrWaitRunningTimeout, naming the services that are not ready
	// All goroutines of the call are finished when it returns.
	if err := c.WaitAllRunning(ctx, 30*time.Second); err != nil {
		var se *service.StopError
		if errors.As(err, &se) {
			// the real reason, e.g. se.Service failed with se.Err
		}
	}
```

`WaitAllRunningTimeout(timeout)` is deprecated. It returns only a bool, does not return when the container stops
and keeps its goroutines until the timeout.

When the container stops, the first trigger is recorded as stop cause. Possible reasons:
`service failed`, `service init failed`, `service panicked`, `service start failed`,
`StopAll called` and `parent context done`. A done parent context always wins, also if `StopAll()` is called right after it.

`OnShutdown` callbacks are called once for every stop path. The services are already canceled when the callbacks run,
so a callback can wait for them with `WaitAllStopped`. Calling `StopAll()` inside a callback is safe.
If the container starts to stop while `StartAll` is still starting services (a service failed or the parent context is done),
the remaining services are not started and `StartAll` returns an error that wraps the stop cause.

A panic in `Init()` or `Run()` is recovered and handled like an error returned by the service.
The error is a `*service.PanicError` that contains the panic value and the stack trace.

## Logging

Set a `*slog.Logger` with `c.SetLogger(logger)` to get lifecycle logs. All lines carry a `container` attribute
(set via `service.NewContainer(service.WithName("core"))`), all service related lines a `service` attribute.

| Level | Message | When |
|-------|---------|------|
| INFO  | `Starting service`, `All services started` | Start |
| ERROR | `Failed to initialize service`, `Service panicked during init` | `Init()` failed |
| WARN  | `Services not started because of init failure` | Lists services that ran `Init()` (`initialized`) and all services that were not started (`not_started`) |
| ERROR | `Service failed`, `Service panicked` | The service that caused the shutdown |
| INFO / WARN | `Stopping all services` | Once per container, with `reason`, `service` and `error`. WARN if a service caused it |
| INFO  | `Service stopped` | Service stopped during shutdown, also when it returned `context.Canceled` |
| WARN  | `Service stopped with error during shutdown` | Follow-up error after the shutdown started, with `error` and `stop_cause` (and `stack` after a panic) |
| WARN  | `Service stopped unexpectedly` | `Run()` returned `nil` before the container was stopped |
| WARN  | `Container is stopping, skip starting remaining services` | A service failed while `StartAll` was still starting services |
| WARN  | `Timeout waiting for services to stop` | `WaitAllStopped` returned because ctx was done, lists `running` services |
| INFO  | `All services stopped` | All services stopped, with `count` (services that ran), `failed` (the stop cause plus services with errors other than context errors) and `stop_cause` |

## Service names

Services have names. Using the builder you just pass the name as string. 
Using a struct to implement `Runner` interface, the service name is derived from the struct name via reflection.
To change this name you can implement the `fmt.Stringer` interface.

## Service initialization

Before any `Run()` method gets called, 
optional `Init()` methods from the `service.Initer` interface are executed sequentially
in oder of service registration.

```
// Initer can be optionally implemented for services that need to run initial startup code
// All init methods of registered services are executed sequentially
// When Init() returns an error, no further services are executed and the application shuts down
type Initer interface {
	Init(ctx context.Context) error
}
```

Or use the builder:

```
	service.New("My Service").
		Init(func(ctx context.Context) error {
			return nil
		}).
		Run(func(ctx context.Context) error {
			// Implement your service here. Try to keep it running, only return fatal errors.
			<-ctx.Done()
			// Gracefully shut down your service here
			return nil
		}).
		Register(c)
```

