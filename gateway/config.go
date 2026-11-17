package gateway

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// Config is the gateway's config file. It holds no secrets, only the names of
// the environment variables that do, so it can be committed.
type Config struct {
	Listen string `yaml:"listen"`
	// ShutdownGrace is how long a shutdown waits for the requests in flight
	// before it cuts them off. YAML gives it as "30s" or "1m"; a bare number
	// is an error, not nanoseconds.
	ShutdownGrace time.Duration         `yaml:"shutdown_grace"`
	Upstreams     []Upstream            `yaml:"upstreams"`
	Models        map[string]ModelRoute `yaml:"models"` // keyed by logical name
	Callers       []Caller              `yaml:"callers"`
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
	// MaxConcurrent is how many requests the upstream gets at once; the rest
	// wait in the gateway. For llama-server, its number of slots.
	MaxConcurrent int `yaml:"max_concurrent"`
	// QueueTimeout is how long a request waits for one of those. Missing
	// means 10 s.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

// defaultQueueTimeout is many extractions' worth of waiting, as each takes
// under a second, and still short enough that a person on Discord does not
// give up first.
const defaultQueueTimeout = 10 * time.Second

// ModelRoute says which upstream serves a logical model, and under what name.
type ModelRoute struct {
	Upstream string   `yaml:"upstream"`
	Model    string   `yaml:"model"`
	Timeouts Timeouts `yaml:"timeouts"`
}

// Timeouts are the four ways a model can be too slow, each with a limit of its
// own. They are set per model, since models differ by orders of magnitude: an
// embedding takes milliseconds, a chat answer with thinking most of a minute.
type Timeouts struct {
	// Connect is for the TCP connection to the upstream.
	Connect time.Duration `yaml:"connect"`
	// FirstByte runs from sending the request to the first byte of the
	// response body. Not the headers: a stream gets those as soon as it has a
	// slot in the engine, before the model has read the prompt.
	FirstByte time.Duration `yaml:"first_byte"`
	// Idle is the longest a stream may go without an event.
	Idle time.Duration `yaml:"idle"`
	// Total is the whole request, from when it arrives: the queue, the
	// upstream, and the whole stream.
	Total time.Duration `yaml:"total"`
}

// Caller is a client of the gateway, such as the cooking app.
type Caller struct {
	Name      string    `yaml:"name"`
	APIKeyEnv string    `yaml:"api_key_env"`
	APIKey    string    `yaml:"-"` // read from APIKeyEnv by LoadConfig
	RateLimit RateLimit `yaml:"rate_limit"`
}

// RateLimit is a caller's token bucket: it may send Burst requests at once,
// and PerSecond on average.
type RateLimit struct {
	Burst     int     `yaml:"burst"`
	PerSecond float64 `yaml:"per_second"`
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

	// 0 would cut every request off at once. A missing value is 0 too, so
	// this also makes the field required.
	if c.ShutdownGrace <= 0 {
		problem("shutdown_grace must be more than 0, got %v", c.ShutdownGrace)
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
		// No default: the right number depends on the server, and a guess
		// that is too high just moves the queue into the engine, where it
		// cannot be seen or timed out.
		if u.MaxConcurrent < 1 {
			problem("%s: max_concurrent must be at least 1, got %d", where, u.MaxConcurrent)
		}
		switch {
		case u.QueueTimeout == 0:
			u.QueueTimeout = defaultQueueTimeout
		case u.QueueTimeout < 0:
			problem("%s: queue_timeout must be more than 0, got %v", where, u.QueueTimeout)
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
		// No defaults: no one value suits a model that answers in
		// milliseconds and one that thinks for a minute.
		t := m.Timeouts
		if t == (Timeouts{}) {
			problem("%s: timeouts is missing", where)
			continue
		}
		for _, f := range []struct {
			name string
			d    time.Duration
		}{{"connect", t.Connect}, {"first_byte", t.FirstByte}, {"idle", t.Idle}, {"total", t.Total}} {
			if f.d <= 0 {
				problem("%s: timeouts.%s must be more than 0, got %v", where, f.name, f.d)
			}
		}
		// Otherwise the total timeout always comes first, and a slow first
		// byte is reported as the wrong kind of timeout.
		if t.FirstByte > t.Total {
			problem("%s: timeouts.first_byte (%v) is longer than timeouts.total (%v)", where, t.FirstByte, t.Total)
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
		// Far wider than any real setting, and narrow enough that every
		// bucket they allow fits in a Duration, so NewTokenBucket never
		// panics on a config that loaded. Written the positive way round so
		// that NaN fails too.
		if !(k.RateLimit.Burst >= 1 && k.RateLimit.Burst <= 1_000_000) {
			problem("%s: rate_limit.burst must be between 1 and 1000000, got %d", where, k.RateLimit.Burst)
		}
		if !(k.RateLimit.PerSecond >= 0.001 && k.RateLimit.PerSecond <= 1_000_000) {
			problem("%s: rate_limit.per_second must be between 0.001 and 1000000, got %v", where, k.RateLimit.PerSecond)
		}
	}
	return errors.Join(errs...)
}
