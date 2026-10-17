package service

import (
	"context"
)

type Builder struct {
	name string
	init InitFunc
	run  RunFunc
}

func New(name string) *Builder {
	b := &Builder{
		name: name,
		// init stays nil unless set via Init(), so no init logs are written for services without init code
		run: func(ctx context.Context) error {
			return nil
		},
	}
	return b
}

func (b *Builder) Init(f InitFunc) *Builder {
	b.init = f
	return b
}

func (b *Builder) Run(f RunFunc) *Builder {
	b.run = f
	return b
}

func (b *Builder) Register(container *Container) {
	container.Register(newGenericService(b.name, b.init, b.run))
}

func (b *Builder) RegisterDefault() {
	Default().Register(newGenericService(b.name, b.init, b.run))
}
