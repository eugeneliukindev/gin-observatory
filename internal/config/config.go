// Package config reads the settings of the process from its environment, once, at startup.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"

	"go-observatory/internal/enums"
)

// The prefix of every variable; groups nest with `__`.
const prefix = "OBSERVATORY__"

// Settings are everything the process needs from outside; a value it does not know is refused.
type Settings struct {
	Server   ServerSettings `envPrefix:"SERVER__"`
	LogLevel enums.LogLevel `env:"LOG_LEVEL" envDefault:"info"`
	DB       DBSettings     `envPrefix:"DB__"`
	Obs      ObsSettings    `envPrefix:"OBS__"`
}

// ServerSettings are where the server listens.
type ServerSettings struct {
	Host string `env:"HOST" envDefault:"127.0.0.1"`
	Port int    `env:"PORT" envDefault:"8000"`
}

// Address is the address as `host:port`.
func (s ServerSettings) Address() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// DBSettings are the SQLite file.
type DBSettings struct {
	Path string `env:"PATH" envDefault:"observatory.db"`
}

// ObsSettings are where telemetry goes and who sends it.
type ObsSettings struct {
	ServiceName string            `env:"SERVICE_NAME" envDefault:"api"`
	Environment enums.Environment `env:"ENVIRONMENT"  envDefault:"development"`
	OTLP        struct {
		Endpoint string `env:"ENDPOINT" envDefault:"http://localhost:4317"`
	} `envPrefix:"OTLP__"`
	Pyroscope struct {
		URL string `env:"URL" envDefault:"http://localhost:4040"`
	} `envPrefix:"PYROSCOPE__"`
	Faro struct {
		// As the browser sees it, not as the server does.
		CollectorURL string `env:"COLLECTOR_URL" envDefault:"http://localhost:12347/collect"`
	} `envPrefix:"FARO__"`
}

// Load reads the settings from the environment.
func Load() (Settings, error) {
	settings, err := env.ParseAsWithOptions[Settings](env.Options{Prefix: prefix})
	if err != nil {
		return Settings{}, fmt.Errorf("settings: %w", err)
	}
	return settings, nil
}
