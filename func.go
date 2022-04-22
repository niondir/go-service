package services

import (
	"context"
	"reflect"
	"runtime"
)

// FuncService is a wrapper that turns a func() into a service.Runner
type FuncService func(ctx context.Context) error

func (f FuncService) Run(ctx context.Context) error {
	return f(ctx)
}

func (f FuncService) String() string {
	return getFunctionName(f)
}

func getFunctionName(i interface{}) string {
	if i == nil {
		return "nil"
	}
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func WithRunFunc(runFn RunFunc) Runner {
	return &genericService{getFunctionName(runFn), nil, runFn}
}

func WithFunc(initFn InitFunc, runFn RunFunc) Runner {
	return &genericService{getFunctionName(runFn), initFn, runFn}
}
