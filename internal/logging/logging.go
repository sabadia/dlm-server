// Package logging builds the application's structured slog logger from the
// boot configuration, writing JSON or text to a size-rotated file and/or
// stderr.
package logging

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/sabadia/dlm-server/internal/config"
)

// FileName is the active log file inside the log directory.
const FileName = config.AppName + ".log"

// Logger bundles the configured logger with its dynamic level and the
// resources that must be closed on shutdown.
type Logger struct {
	*slog.Logger

	// Level can be changed at runtime.
	Level *slog.LevelVar
	// Ring keeps the latest records in memory for the logs API.
	Ring *Ring

	closer io.Closer
}

// Close flushes and closes the log file, if any.
func (l *Logger) Close() error {
	if l.closer == nil {
		return nil
	}
	if err := l.closer.Close(); err != nil {
		return fmt.Errorf("logging: close: %w", err)
	}
	return nil
}

// New creates a logger from cfg. Log files are written to logDir. stderr is
// used when cfg.Stderr is true, or as a fallback when file logging is off.
func New(cfg config.LogConfig, logDir string, stderr io.Writer) (*Logger, error) {
	level := new(slog.LevelVar)
	lvl, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	level.Set(lvl)

	var (
		writers []io.Writer
		closer  io.Closer
	)
	if cfg.File {
		lj := &lumberjack.Logger{
			Filename:   filepath.Join(logDir, FileName),
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   true,
		}
		writers = append(writers, lj)
		closer = lj
	}
	if cfg.Stderr || len(writers) == 0 {
		writers = append(writers, stderr)
	}

	out := io.MultiWriter(writers...)
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "text":
		handler = slog.NewTextHandler(out, opts)
	case "json", "":
		handler = slog.NewJSONHandler(out, opts)
	default:
		return nil, fmt.Errorf("logging: unknown format %q", cfg.Format)
	}
	ring := NewRing(DefaultRingSize)
	return &Logger{Logger: slog.New(NewTeeHandler(handler, ring)), Level: level, Ring: ring, closer: closer}, nil
}

// ParseLevel converts a level name (debug, info, warn, error) to slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, errors.Join(fmt.Errorf("logging: invalid level %q", s), err)
	}
	return l, nil
}

// Discard returns a logger that drops everything; useful in tests.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
