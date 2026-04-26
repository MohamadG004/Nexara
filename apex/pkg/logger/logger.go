package logger

import (
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// New creates a configured zerolog.Logger.
// Production: JSON output (structured, machine-parseable, compatible with Datadog/Loki).
// Development: pretty console output (human-readable with colors and timestamps).
func New(level, env string) *zerolog.Logger {
	zerolog.TimeFieldFormat = time.RFC3339Nano

	var w io.Writer
	if env == "development" {
		// Pretty-print for local development
		w = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "15:04:05",
		}
	} else {
		w = os.Stdout
	}

	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	log := zerolog.New(w).
		Level(lvl).
		With().
		Timestamp().
		Caller(). // includes file:line — great for debugging
		Logger()

	return &log
}
