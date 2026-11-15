package app

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Config is the app's config. All of it comes from the environment, which
// .env.example documents.
type Config struct {
	GatewayURL   string
	AppKey       string // the app's key for the gateway
	DiscordToken string
	AllowedUsers []string // Discord user IDs
	// Location is where "today" and "yesterday" are, for the dates in the
	// log.
	Location *time.Location
	DBPath   string
}

// ConfigFromEnv reads the config through getenv, which is os.Getenv except
// in tests. The error lists every problem, and never a secret's value.
func ConfigFromEnv(getenv func(string) string) (*Config, error) {
	var errs []error
	problem := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	required := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			problem("%s is not set", name)
		}
		return v
	}

	c := &Config{
		GatewayURL:   strings.TrimSuffix(strings.TrimSpace(getenv("SOUS_GATEWAY_URL")), "/"),
		AppKey:       required("SOUS_APP_KEY"),
		DiscordToken: required("SOUS_DISCORD_TOKEN"),
		DBPath:       strings.TrimSpace(getenv("SOUS_DB")),
	}
	if c.GatewayURL == "" {
		c.GatewayURL = "http://127.0.0.1:8080"
	}
	if u, err := url.Parse(c.GatewayURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problem("SOUS_GATEWAY_URL %q is not an http:// or https:// URL", c.GatewayURL)
	}

	if users := required("SOUS_DISCORD_USERS"); users != "" {
		for _, id := range strings.Split(users, ",") {
			id = strings.TrimSpace(id)
			// User IDs are numbers; a name here would never match anyone.
			if id == "" || strings.Trim(id, "0123456789") != "" {
				problem("SOUS_DISCORD_USERS: %q is not a Discord user ID", id)
				continue
			}
			c.AllowedUsers = append(c.AllowedUsers, id)
		}
	}

	// Required rather than the machine's zone: in a container that is UTC,
	// and every evening's cooking would land on the next day.
	if zone := required("SOUS_TIMEZONE"); zone != "" {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			problem("SOUS_TIMEZONE %q is not a time zone such as America/Chicago", zone)
		}
		c.Location = loc
	}

	if c.DBPath == "" {
		// Outside the repository, and on Linux's own file system rather than
		// under /mnt/c, which is slower and shares files with Windows.
		data := getenv("XDG_DATA_HOME")
		if data == "" {
			if home := getenv("HOME"); home != "" {
				data = filepath.Join(home, ".local", "share")
			}
		}
		if data == "" {
			problem("SOUS_DB is not set, and neither is HOME")
		} else {
			c.DBPath = filepath.Join(data, "sous", "sous.db")
		}
	}
	return c, errors.Join(errs...)
}
