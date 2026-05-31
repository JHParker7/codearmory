package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func loadConfig() config {
	cfg := config{URL: "http://localhost:8082"}

	if p := configFilePath(); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			var file config
			if json.Unmarshal(data, &file) == nil {
				if file.URL != "" {
					cfg.URL = file.URL
				}
				cfg.Token = file.Token
			}
		}
	}

	if v := os.Getenv("CODEARMORY_URL"); v != "" {
		cfg.URL = v
	}
	if v := os.Getenv("CODEARMORY_TOKEN"); v != "" {
		cfg.Token = v
	}
	return cfg
}

func configFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "codearmory", "config.json")
}

func (c config) validate() error {
	if c.Token == "" {
		return errors.New("no token configured — set CODEARMORY_TOKEN or run `armory auth login`")
	}
	return nil
}
