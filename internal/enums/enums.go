// Package enums holds the closed sets of values the process reads from outside.
package enums

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// LogLevel is a slog level read by its name in any case; WARNING is the name the logs carry.
type LogLevel struct{ slog.Level }

// UnmarshalText reads DEBUG, INFO, WARNING or WARN, ERROR.
func (l *LogLevel) UnmarshalText(text []byte) error {
	name := strings.ToUpper(string(text))
	if name == "WARNING" {
		name = "WARN"
	}
	return l.Level.UnmarshalText([]byte(name))
}

// Environment is where the process runs.
type Environment string

var environments = []Environment{"development", "staging", "production", "test"}

// UnmarshalText takes one of the known environments only.
func (e *Environment) UnmarshalText(text []byte) error {
	value := Environment(text)
	if !slices.Contains(environments, value) {
		return fmt.Errorf("%q: want one of %v", value, environments)
	}
	*e = value
	return nil
}
