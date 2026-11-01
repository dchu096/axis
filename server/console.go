package server

import (
	"strings"
	"time"

	"github.com/mitchellh/colorstring"

	"github.com/0x7d8/wings/config"
	"github.com/0x7d8/wings/system"
)

// MessageProcessor defines an interface for processing messages.
type MessageProcessor interface {
	Process(data string) string
}

// ReplacementProcessor is a processor that replaces specific log messages based on predefined settings.
type ReplacementProcessor struct {
	settings map[string]string
}

// NewReplacementProcessor creates a new ReplacementProcessor with the given settings.
func NewReplacementProcessor(settings map[string]string) *ReplacementProcessor {
	return &ReplacementProcessor{settings: settings}
}

// Process replaces specific log messages based on predefined settings.
func (rp *ReplacementProcessor) Process(data string) string {
	for original, replacement := range rp.settings {
		if strings.Contains(data, original) {
			return strings.ReplaceAll(data, original, replacement)
		}
	}
	return data
}

// Left side is the message the daemon publishes. Right side is what the console shows.
var settings = map[string]string{
	"Checking server disk space usage, this could take a few seconds...":                  "Checking disk space. This can take a few seconds...",
	"Updating process configuration files...":                                             "Updating server configuration...",
	"Ensuring file permissions are set correctly, this could take a few seconds...":       "Setting file permissions. This can take a few seconds...",
	"Pulling Docker container image, this could take a few minutes to complete...":        "Pulling the server image. This can take a few minutes...",
	"Finished pulling Docker container image":                                             "Server image is ready.",
	"Server crash was detected but an error occurred while handling it.":                  "The server crashed, and Axis could not restart it.",
	"Server is outputting console data too quickly -- throttling...":                      "Console output is too fast. Slowing it down...",
	"Server is exceeding the assigned disk space limit, stopping process now.":            "Disk limit reached. Stopping the server.",
	"Aborting automatic restart, crash detection is disabled for this instance.":          "Crash recovery is off. Axis will not restart this server.",
	"---------- Detected server process in a crashed state! ----------":                   "The server process crashed.",
	"Aborting automatic restart, last crash occurred less than ":                          "Holding restart. Last crash was less than ",
}

// daemonPrefix is the console label for daemon-originated lines.
const daemonPrefix = "[light_blue]axis:[default] "

// PublishConsoleOutputFromDaemon sends output to the server console formatted
// to appear correctly as being sent from Axis.
func (s *Server) PublishConsoleOutputFromDaemon(data string) {
	// Create the message processors
	processors := []MessageProcessor{
		NewReplacementProcessor(settings),
	}

	// Process the message through all processors
	for _, processor := range processors {
		data = processor.Process(data)
	}
	formattedData := daemonPrefix + data

	// Publish the formatted data to the server console
	s.Events().Publish(
		ConsoleOutputEvent,
		colorstring.Color(formattedData),
	)
}

// Throttler returns the throttler instance for the server or creates a new one.
func (s *Server) Throttler() *ConsoleThrottle {
	s.throttleOnce.Do(func() {
		throttles := config.Get().Throttles
		period := time.Duration(throttles.Period) * time.Millisecond

		s.throttler = newConsoleThrottle(throttles.Lines, period)
		s.throttler.strike = func() {
			s.PublishConsoleOutputFromDaemon("Server is outputting console data too quickly -- throttling...")
		}
	})
	return s.throttler
}

type ConsoleThrottle struct {
	limit  *system.Rate
	lock   *system.Locker
	strike func()
}

func newConsoleThrottle(lines uint64, period time.Duration) *ConsoleThrottle {
	return &ConsoleThrottle{
		limit: system.NewRate(lines, period),
		lock:  system.NewLocker(),
	}
}

// Allow checks if the console is allowed to process more output data, or if too
// much has already been sent over the line. If there is too much output the
// strike callback function is triggered, but only if it has not already been
// triggered at this point in the process.
//
// If output is allowed, the lock on the throttler is released and the next time
// it is triggered the strike function will be re-executed.
func (ct *ConsoleThrottle) Allow() bool {
	if !ct.limit.Try() {
		if err := ct.lock.Acquire(); err == nil {
			if ct.strike != nil {
				ct.strike()
			}
		}
		return false
	}
	ct.lock.Release()
	return true
}

// Reset resets the console throttler internal rate limiter and overage counter.
func (ct *ConsoleThrottle) Reset() {
	ct.limit.Reset()
}
