package app

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/GJH1203/sous/app/bot"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

var validEnv = map[string]string{
	"SOUS_APP_KEY":       "app-secret",
	"SOUS_DISCORD_TOKEN": "token-secret",
	"SOUS_DISCORD_USERS": "123456789012345678, 876543210987654321",
	"SOUS_TIMEZONE":      "America/Chicago",
	"HOME":               "/home/cook",
}

func TestConfigFromEnv(t *testing.T) {
	c, err := ConfigFromEnv(env(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if c.GatewayURL != "http://127.0.0.1:8080" || c.AppKey != "app-secret" || c.DiscordToken != "token-secret" ||
		!slices.Equal(c.AllowedUsers, []string{"123456789012345678", "876543210987654321"}) ||
		c.Location.String() != "America/Chicago" || c.DBPath != "/home/cook/.local/share/sous/sous.db" {
		t.Errorf("got %+v", c)
	}
}

func TestConfigOverrides(t *testing.T) {
	vars := with(validEnv, map[string]string{
		"SOUS_GATEWAY_URL": "https://gateway.example.com/",
		"XDG_DATA_HOME":    "/data",
	})
	c, err := ConfigFromEnv(env(vars))
	if err != nil || c.GatewayURL != "https://gateway.example.com" || c.DBPath != "/data/sous/sous.db" {
		t.Fatalf("got %+v, %v", c, err)
	}
	vars["SOUS_DB"] = "/tmp/x.db"
	if c, _ := ConfigFromEnv(env(vars)); c.DBPath != "/tmp/x.db" {
		t.Errorf("SOUS_DB ignored: %s", c.DBPath)
	}
}

func TestConfigReportsEveryProblem(t *testing.T) {
	vars := with(validEnv, map[string]string{
		"SOUS_APP_KEY":       "",
		"SOUS_DISCORD_TOKEN": "  ",
		"SOUS_GATEWAY_URL":   "127.0.0.1:8080",
		"SOUS_DISCORD_USERS": "123,alice,",
		"SOUS_TIMEZONE":      "Mars/Olympus",
		"HOME":               "",
	})
	_, err := ConfigFromEnv(env(vars))
	want := strings.Join([]string{
		"SOUS_APP_KEY is not set",
		"SOUS_DISCORD_TOKEN is not set",
		`SOUS_GATEWAY_URL "127.0.0.1:8080" is not an http:// or https:// URL`,
		`SOUS_DISCORD_USERS: "alice" is not a Discord user ID`,
		`SOUS_DISCORD_USERS: "" is not a Discord user ID`,
		`SOUS_TIMEZONE "Mars/Olympus" is not a time zone such as America/Chicago`,
		"SOUS_DB is not set, and neither is HOME",
	}, "\n")
	if err == nil || err.Error() != want {
		t.Errorf("got\n%v\nwant\n%s", err, want)
	}
}

// time.LoadLocation("") is UTC, without an error, so only the check for an
// empty value keeps a missing zone from quietly becoming UTC.
func TestTimezoneIsRequired(t *testing.T) {
	_, err := ConfigFromEnv(env(with(validEnv, map[string]string{"SOUS_TIMEZONE": ""})))
	if err == nil || err.Error() != "SOUS_TIMEZONE is not set" {
		t.Errorf("got %v", err)
	}
}

func TestConfigErrorsHoldNoSecrets(t *testing.T) {
	vars := with(validEnv, map[string]string{"SOUS_TIMEZONE": "", "SOUS_DISCORD_USERS": "x"})
	_, err := ConfigFromEnv(env(vars))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("got %v", err)
	}
}

func TestHandleAnswers(t *testing.T) {
	a := New(nil, nil, nil, nil)
	if reply := a.Handle(context.Background(), bot.Message{Content: "你好"}); reply == "" {
		t.Error("no reply")
	}
}

// with returns a copy of base with changes applied.
func with(base, changes map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, changes)
	return out
}
