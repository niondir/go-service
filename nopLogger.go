package service

import (
	"context"
	"log/slog"
)

var _ slog.Handler = NopHandler{}

type NopHandler struct {
}

func (n NopHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return false
}

func (n NopHandler) Handle(ctx context.Context, record slog.Record) error {
	return nil
}

func (n NopHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return n
}

func (n NopHandler) WithGroup(name string) slog.Handler {
	return n
}
