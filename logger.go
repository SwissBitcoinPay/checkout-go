package checkout

import "log/slog"

// Logger is where warnings go (a format newer than this package reads, a rate source that failed
// before the next one gave a price, a record that could not be published and is published again
// at the next check). Never given a secret. Warn is called from several goroutines at once: it
// must be safe for concurrent use, as slog's is.
type Logger interface {
	Warn(message string)
}

// LoggerFunc is a function as a Logger.
type LoggerFunc func(message string)

// Warn calls f.
func (f LoggerFunc) Warn(message string) { f(message) }

// defaultLogger writes warnings to slog's default logger.
type defaultLogger struct{}

func (defaultLogger) Warn(message string) { slog.Warn(message) }
