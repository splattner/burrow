package logging

import (
	"io"
	"os"

	"github.com/sirupsen/logrus"
)

// New returns a configured logrus.Logger with text formatting.
// level must be a logrus level string (debug, info, warn, error, fatal, panic).
// Unknown values default to info.
func New(level string) *logrus.Logger {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	parsed, err := logrus.ParseLevel(level)
	if err != nil {
		parsed = logrus.InfoLevel
	}
	log.SetLevel(parsed)
	return log
}

// NoOp returns a logrus.Logger that discards all output. Suitable for tests.
func NoOp() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}
