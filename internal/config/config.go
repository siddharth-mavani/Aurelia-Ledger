package config

import (
	"fmt"
	"os"
)

// Config contains only process configuration; credentials stay in environment variables.
type Config struct {
	DatabaseURL string
	ListenAddr  string
	APIToken    string
}

func Load() (Config, error) {
	config := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ListenAddr:  os.Getenv("LISTEN_ADDR"),
		APIToken:    os.Getenv("AURELIA_LEDGER_API_TOKEN"),
	}
	if config.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if config.ListenAddr == "" {
		config.ListenAddr = ":8080"
	}
	return config, nil
}
