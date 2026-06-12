// Package envfile loads a .env file into the process environment at startup.
// Zero dependencies, no override: a variable already set in the real
// environment (e.g. on Railway) always wins over the file, so the same
// binary behaves correctly locally and in production.
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// Load reads KEY=VALUE lines from the named file (default ".env") if it
// exists. Missing file is not an error — production sets real env vars.
// Lines that are blank or start with # are ignored; surrounding quotes on
// the value are stripped; existing environment variables are never replaced.
func Load(paths ...string) {
	if len(paths) == 0 {
		paths = []string{".env"}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // no .env here — fine
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Allow an optional leading "export ".
			line = strings.TrimPrefix(line, "export ")
			eq := strings.IndexByte(line, '=')
			if eq < 0 {
				continue
			}
			key := strings.TrimSpace(line[:eq])
			val := strings.TrimSpace(line[eq+1:])
			if key == "" {
				continue
			}
			// Strip matching surrounding quotes.
			if len(val) >= 2 {
				if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
					val = val[1 : len(val)-1]
				}
			}
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, val)
			}
		}
		f.Close()
	}
}
