# go Services

A package to manage background services in go applications.


# Usage


## Implement your service 

A service is as simple as implementing the `Runner` interface:

```
type Runner interface {
    // Run is executed inside it's own go-routine and must not return until the service stops.
	// Run should only return after ctx.Done() or when a non recoverable error occurs.
	// Returning an error means the service did fail. On ctx.Done() the service should shutdown gracefully.
    Run(ctx context.Context) error
}
```

And register them inside a container:

```
	c := services.NewContainer() // or use services.Default()
	c.Register(s1)
```

There is also a builder pattern if you prefer not to implement the interface yourself:

```
	c := services.NewContainer()
	
	services.New("My Service").Run(func(ctx context.Context) error {
		// Implement your service here. Try to keep it running, only return fatal errors.
		<-ctx.Done()
		// Gracefully shut down your service here
		return nil
	}).Register(c)
```

Service names must be unique inside a single container.

## Start and Stop your services

After registering all services you can start them all together.

```
	runCtx, runCtxCancel := context.WithCancel(context.Background())
	defer runCtxCancel()
	err := c.StartAll(runCtx)
	// err comes from the initialization (see below)
```

Stop all services, by either calling `c.StopAll()` or `runCtxCancel()`.
All services also stop if any `Run()` function returns with or without an error.

You can actively wait for all services to stop:

```
	c.WaitAllStopped(context.Background())

    // You can check for any errors that might have caused the services to stop
	errs := c.ServiceErrors()
```

## Service names

Services have names. Using the builder you just pass the name as string. 
Using a struct to implement `Runner` interface, the service name is derived from the struct name via reflection.
To change this name you can implement the `fmt.Stringer` interface.

## Service initialization

Before any `Run()` method gets called, 
optional `Init()` methods from the `services.Initer` interface are executed sequentially
in oder of service registration.

```
// Initer can be optionally implemented for services that need to run initial startup code
// All init methods of registered services are executed sequentially
// When a starter returns an error, no further services are executed and the application shuts down
type Initer interface {
	Init(ctx context.Context) error
}
```

Or use the builder:

```
	services.New("My Service").
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

