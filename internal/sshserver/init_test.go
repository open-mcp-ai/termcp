package sshserver

import (
	"io"
	"log/slog"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}
