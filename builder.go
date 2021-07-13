package services

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
		init: func(ctx context.Context) error {
			return nil
		},
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
	container.Register(&startRunner{b.name, b.init, b.run})
}

func (b *Builder) RegisterDefault() {
	Default().Register(&startRunner{b.name, b.init, b.run})
}
