package darepo

import (
	"os"

	"github.com/btcsuite/btclog/v2"
)

func (s *Server) setupLogging() error {
	// Simple logger setup for now - TODO: add proper log rotation
	handler := btclog.NewDefaultHandler(os.Stdout)
	logger := btclog.NewSLogger(handler)

	s.loggerFactory = func(subsystem string) btclog.Logger {
		return logger
	}

	s.log = logger

	return nil
}
