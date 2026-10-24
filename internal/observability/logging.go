package observability

import (
	"log/slog"
	"os"
)

// Logging holds a service's immutable logger. Its zero value logs at Info level.
type Logging struct {
	logger *slog.Logger
}

// NewLogging uses the supplied logger, or a text logger with the configured level.
// Injected handlers control their own levels and must be safe for concurrent use.
func NewLogging(debug bool, loggers ...*slog.Logger) Logging {
	if len(loggers) > 0 && loggers[0] != nil {
		return Logging{logger: loggers[0]}
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return Logging{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))}
}

func (logging Logging) Log() *slog.Logger {
	if logging.logger == nil {
		return NewLogging(false).logger
	}
	return logging.logger
}
