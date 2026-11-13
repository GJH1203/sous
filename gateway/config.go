package gateway

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Config is the gateway's config file. It holds no secrets, only the names of
// the environment variables that do, so it can be committed.
type Config struct {
	Listen    string                `yaml:"listen"`
	Upstreams []Upstream            `yaml:"upstreams"`
	Models    map[string]ModelRoute `yaml:"models"` // keyed by logical name
	Callers   []Caller              `yaml:"callers"`
}

// Upstream is a server that speaks the OpenAI-compatible API.
type Upstream struct {
	Name string `yaml:"name"`
	// BaseURL comes without /v1: the paths the gateway forwards include it.
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the variable that holds the gateway's key for this
	// upstream. Local servers need none.
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"` // read from APIKeyEnv by LoadConfig
}

// ModelRoute says which upstream serves a logical model, and under what name.
type ModelRoute struct {
	Upstream string `yaml:"upstream"`
	Model    string `yaml:"model"`
}

// Caller is a client of the gateway, such as the cooking app.
type Caller struct {
	Name      string `yaml:"name"`
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"` // read from APIKeyEnv by LoadConfig
}

// LoadConfig reads and checks the config file, and reads the keys it names
// from the environment. The error lists every problem, not just the first,
// so that one edit can fix them all.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	// Without WithKnownFields, a misspelled key is skipped without a word and
	// its field is left empty.
	if err := yaml.Load(data, &c, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s:\n%w", path, err)
	}
	return &c, nil
}

// check validates c and fills in the keys from the environment.
func (c *Config) check() error {
	var errs []error
	problem := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	// named checks that the i-th entry of a list has a name of its own.
	named := func(seen map[string]int, list string, i int, name string) {
		j, dup := seen[name]
		switch {
		case name == "":
			problem("%s[%d]: name is missing", list, i)
		case dup:
			problem("%s[%d]: name %q is already used by %s[%d]", list, i, name, list, j)
		default:
			seen[name] = i
		}
	}
	secret := func(where, env string) string {
		key := os.Getenv(env)
		if key == "" {
			problem("%s: environment variable %s is not set", where, env)
		}
		return key
	}

	if c.Listen == "" {
		problem("listen is missing")
	}

	upstreams := make(map[string]int)
	for i := range c.Upstreams {
		// A pointer into the slice. With "for _, u := range", u would be a
		// copy, and the key stored in it would be lost.
		u := &c.Upstreams[i]
		where := fmt.Sprintf("upstreams[%d]", i)
		named(upstreams, "upstreams", i, u.Name)
		u.BaseURL = strings.TrimSuffix(u.BaseURL, "/")
		// url.Parse accepts almost anything: "localhost:8081" parses, with
		// "localhost" as the scheme.
		base, err := url.Parse(u.BaseURL)
		if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
			problem("%s: base_url %q is not an http:// or https:// URL", where, u.BaseURL)
		}
		if u.APIKeyEnv != "" {
			u.APIKey = secret(where, u.APIKeyEnv)
		}
	}

	if len(c.Models) == 0 {
		problem("models: none are defined")
	}
	// Sorted, because the order of a map changes from run to run.
	for _, name := range slices.Sorted(maps.Keys(c.Models)) {
		m := c.Models[name]
		where := "models." + name
		if _, ok := upstreams[m.Upstream]; !ok {
			problem("%s: upstream %q is not defined", where, m.Upstream)
		}
		if m.Model == "" {
			problem("%s: model is missing", where)
		}
	}

	callers := make(map[string]int)
	for i := range c.Callers {
		k := &c.Callers[i]
		where := fmt.Sprintf("callers[%d]", i)
		named(callers, "callers", i, k.Name)
		if k.APIKeyEnv == "" {
			problem("%s: api_key_env is missing", where)
		} else {
			k.APIKey = secret(where, k.APIKeyEnv)
		}
	}
	return errors.Join(errs...)
}
