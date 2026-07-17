// Package config loads application configuration from environment variables
// and optional files. It is the single source of runtime configuration.
package config

import (
	"fmt"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config is the root application configuration.
type Config struct {
	Env  string `envconfig:"ENV" default:"dev"` // dev | staging | prod
	HTTP HTTP
	DB   DB
	Log  Log
}

// HTTP configures the gateway server.
type HTTP struct {
	Addr            string        `envconfig:"HTTP_ADDR" default:":8080"`
	ReadTimeout     time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"30s"`
	WriteTimeout    time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"60s"`
	ShutdownTimeout time.Duration `envconfig:"HTTP_SHUTDOWN_TIMEOUT" default:"15s"`
}

// DB configures Postgres.
type DB struct {
	DSN             string `envconfig:"DB_DSN" default:"postgres://postgres:postgres@localhost:5432/nowhere?sslmode=disable"`
	MaxOpenConns    int    `envconfig:"DB_MAX_OPEN_CONNS" default:"20"`
	MaxIdleConns    int    `envconfig:"DB_MAX_IDLE_CONNS" default:"5"`
	ConnMaxLifetime time.Duration `envconfig:"DB_CONN_MAX_LIFETIME" default:"30m"`
}

// Log configures logging.
type Log struct {
	Level  string `envconfig:"LOG_LEVEL" default:"debug"` // debug|info|warn|error
	Format string `envconfig:"LOG_FORMAT" default:"text"`  // text|json
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return Config{}, fmt.Errorf("process env config: %w", err)
	}
	return c, nil
}
