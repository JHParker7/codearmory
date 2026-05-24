package main

import (
	"log/slog"
	"os"
	"strings"
)

// secret reads the named environment variable. If <NAME>_FILE is set, the
// value is read from that file instead (trailing whitespace stripped), so that
// Docker Compose secrets mounts (/run/secrets/) and Kubernetes Secret volumes
// work without any code changes. The file path takes precedence over the plain
// env var. If the file is specified but unreadable the process exits
// immediately rather than starting with a missing credential.
func secret(name string) string {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("cannot read secret file", "var", name+"_FILE", "path", path, "error", err)
			os.Exit(1)
		}
		return strings.TrimSpace(string(data))
	}
	return os.Getenv(name)
}
