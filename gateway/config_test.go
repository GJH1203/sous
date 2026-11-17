package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GJH1203/sous/gateway/ratelimit"
)

// writeConfig writes a config file to a fresh temporary directory.
func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The line numbers in TestConfigProblems count from this first, empty line.
const validConfig = `
listen: 127.0.0.1:8080
upstreams:
  - name: local
    base_url: http://127.0.0.1:8081/
    max_concurrent: 4
  - name: cloud
    base_url: https://api.example.com/compat
    api_key_env: TEST_CLOUD_KEY
    max_concurrent: 8
    queue_timeout: 30s
models:
  chat: {upstream: cloud, model: big-model, timeouts: {connect: 3s, first_byte: 1m, idle: 15s, total: 3m}}
  embed: {upstream: local, model: small-embedder, timeouts: {connect: 1s, first_byte: 5s, idle: 5s, total: 15s}}
callers:
  - name: app
    api_key_env: TEST_APP_KEY
    rate_limit: {burst: 20, per_second: 0.5}
shutdown_grace: 1m30s
`

func setKeys(t *testing.T) {
	t.Setenv("TEST_CLOUD_KEY", "cloud-secret")
	t.Setenv("TEST_APP_KEY", "app-secret")
}

func TestLoadConfig(t *testing.T) {
	setKeys(t)
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Listen:        "127.0.0.1:8080",
		ShutdownGrace: 90 * time.Second,
		Upstreams: []Upstream{
			// Without the trailing slash, and with the default queue timeout.
			{Name: "local", BaseURL: "http://127.0.0.1:8081", MaxConcurrent: 4, QueueTimeout: 10 * time.Second},
			{Name: "cloud", BaseURL: "https://api.example.com/compat", APIKeyEnv: "TEST_CLOUD_KEY", APIKey: "cloud-secret",
				MaxConcurrent: 8, QueueTimeout: 30 * time.Second},
		},
		Models: map[string]ModelRoute{
			"chat": {Upstream: "cloud", Model: "big-model",
				Timeouts: Timeouts{Connect: 3 * time.Second, FirstByte: time.Minute, Idle: 15 * time.Second, Total: 3 * time.Minute}},
			"embed": {Upstream: "local", Model: "small-embedder",
				Timeouts: Timeouts{Connect: time.Second, FirstByte: 5 * time.Second, Idle: 5 * time.Second, Total: 15 * time.Second}},
		},
		Callers: []Caller{{Name: "app", APIKeyEnv: "TEST_APP_KEY", APIKey: "app-secret",
			RateLimit: RateLimit{Burst: 20, PerSecond: 0.5}}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got  %+v\nwant %+v", *cfg, *want)
	}
}

// The committed config files must stay loadable.
func TestCommittedConfigs(t *testing.T) {
	t.Setenv("SOUS_APP_KEY", "test-key")
	for _, name := range []string{"gateway.yaml", "gateway.fake.yaml"} {
		cfg, err := LoadConfig(filepath.Join("..", "config", name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, model := range []string{"extract", "chat", "embed"} {
			if _, ok := cfg.Models[model]; !ok {
				t.Errorf("%s: no %s model", name, model)
			}
		}
	}
}

func TestConfigProblems(t *testing.T) {
	tests := []struct {
		name     string
		old, new string // an edit to validConfig
		unset    string // an environment variable to leave empty
		want     string
	}{
		{"misspelled key", "base_url: https", "base_ur: https", "", "line 8: field base_ur not found"},
		{"duplicate key", "  embed:", "  chat: {upstream: local, model: x}\n  embed:", "", `line 14: mapping key "chat" already defined at line 13`},
		{"no listen", "listen: 127.0.0.1:8080\n", "", "", "listen is missing"},
		{"no scheme", "http://127.0.0.1:8081/", "127.0.0.1:8081", "", `upstreams[0]: base_url "127.0.0.1:8081" is not an http:// or https:// URL`},
		{"host as scheme", "http://127.0.0.1:8081/", "localhost:8081", "", `upstreams[0]: base_url "localhost:8081" is not`},
		{"other scheme", "http://127.0.0.1:8081/", "tcp://127.0.0.1:8081", "", `upstreams[0]: base_url "tcp://127.0.0.1:8081" is not`},
		{"one slash", "http://127.0.0.1:8081/", "http:/127.0.0.1:8081", "", `upstreams[0]: base_url "http:/127.0.0.1:8081" is not`},
		{"unnamed upstream", "- name: cloud\n    base_url", "- base_url", "", "upstreams[1]: name is missing"},
		{"upstream named twice", "name: cloud", "name: local", "", `upstreams[1]: name "local" is already used by upstreams[0]`},
		{"upstream key unset", "", "", "TEST_CLOUD_KEY", "upstreams[1]: environment variable TEST_CLOUD_KEY is not set"},
		{"unknown upstream", "upstream: cloud", "upstream: clod", "", `models.chat: upstream "clod" is not defined`},
		{"no model name", " model: big-model,", "", "", "models.chat: model is missing"},
		{"no models", "models:\n  chat: {upstream: cloud, model: big-model, timeouts: {connect: 3s, first_byte: 1m, idle: 15s, total: 3m}}\n  embed: {upstream: local, model: small-embedder, timeouts: {connect: 1s, first_byte: 5s, idle: 5s, total: 15s}}\n", "", "", "models: none are defined"},
		{"caller key unset", "", "", "TEST_APP_KEY", "callers[0]: environment variable TEST_APP_KEY is not set"},
		{"caller without key", "    api_key_env: TEST_APP_KEY\n", "", "", "callers[0]: api_key_env is missing"},
		{"caller named twice", "callers:\n", "callers:\n  - name: app\n    api_key_env: TEST_APP_KEY\n", "", `callers[1]: name "app" is already used by callers[0]`},
		{"no grace", "shutdown_grace: 1m30s\n", "", "", "shutdown_grace must be more than 0, got 0s"},
		{"negative grace", "1m30s", "-1s", "", "shutdown_grace must be more than 0, got -1s"},
		{"grace without a unit", "1m30s", "30", "", "line 19: cannot construct !!int `30` into time.Duration"},
		{"no max_concurrent", "    max_concurrent: 4\n", "", "", "upstreams[0]: max_concurrent must be at least 1, got 0"},
		{"negative max_concurrent", "max_concurrent: 8", "max_concurrent: -1", "", "upstreams[1]: max_concurrent must be at least 1, got -1"},
		{"negative queue_timeout", "queue_timeout: 30s", "queue_timeout: -1s", "", "upstreams[1]: queue_timeout must be more than 0, got -1s"},
		{"no rate_limit", "    rate_limit: {burst: 20, per_second: 0.5}\n", "", "", "callers[0]: rate_limit.burst must be between 1 and 1000000, got 0"},
		{"no per_second", "per_second: 0.5", "per_second: 0", "", "callers[0]: rate_limit.per_second must be between 0.001 and 1000000, got 0"},
		{"burst too big", "burst: 20", "burst: 1000001", "", "callers[0]: rate_limit.burst must be between 1 and 1000000, got 1000001"},
		{"rate too low", "per_second: 0.5", "per_second: 0.0009", "", "rate_limit.per_second must be between 0.001 and 1000000, got 0.0009"},
		{"rate too high", "per_second: 0.5", "per_second: 1e7", "", "rate_limit.per_second must be between 0.001 and 1000000, got 1e+07"},
		{"rate NaN", "per_second: 0.5", "per_second: .nan", "", "rate_limit.per_second must be between 0.001 and 1000000, got NaN"},
		{"rate infinite", "per_second: 0.5", "per_second: .inf", "", "rate_limit.per_second must be between 0.001 and 1000000, got +Inf"},
		{"no timeouts", ", timeouts: {connect: 1s, first_byte: 5s, idle: 5s, total: 15s}", "", "", "models.embed: timeouts is missing"},
		{"no idle timeout", "idle: 15s, ", "", "", "models.chat: timeouts.idle must be more than 0, got 0s"},
		{"negative connect timeout", "connect: 3s", "connect: -1s", "", "models.chat: timeouts.connect must be more than 0, got -1s"},
		{"first byte after total", "total: 3m", "total: 30s", "", "models.chat: timeouts.first_byte (1m0s) is longer than timeouts.total (30s)"},
		{"misspelled timeout", "first_byte: 5s", "firstbyte: 5s", "", "field firstbyte not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setKeys(t)
			if tt.unset != "" {
				t.Setenv(tt.unset, "")
			}
			text := strings.Replace(validConfig, tt.old, tt.new, 1)
			if tt.old != "" && text == validConfig {
				t.Fatalf("the edit %q did not apply", tt.old)
			}
			path := writeConfig(t, text)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("no error; want %q", tt.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.want) || !strings.Contains(msg, path) {
				t.Errorf("error:\n%s\nwant the file name and %q", msg, tt.want)
			}
			if strings.Contains(msg, "secret") {
				t.Errorf("a key's value is in the error:\n%s", msg)
			}
		})
	}
}

// The rate limits the config check lets through, at their extremes, are all
// buckets that NewTokenBucket makes without a panic.
func TestRateLimitBoundsMakeBuckets(t *testing.T) {
	for _, burst := range []int{1, 1_000_000} {
		for _, rate := range []float64{0.001, 1_000_000} {
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Errorf("burst %d, per_second %v: %v", burst, rate, v)
					}
				}()
				ratelimit.NewTokenBucket(burst, rate, time.Now())
			}()
		}
	}
}

// Every problem is reported, one per line, in the same order every time.
func TestConfigReportsEveryProblem(t *testing.T) {
	path := writeConfig(t, `
upstreams:
  - name: a
    base_url: 127.0.0.1:8081
models:
  extract: {upstream: b, model: m}
  chat: {upstream: c, model: m}
  embed: {upstream: d, model: m}
`)
	want := path + ":\n" + strings.Join([]string{
		"listen is missing",
		"shutdown_grace must be more than 0, got 0s",
		`upstreams[0]: base_url "127.0.0.1:8081" is not an http:// or https:// URL`,
		"upstreams[0]: max_concurrent must be at least 1, got 0",
		`models.chat: upstream "c" is not defined`,
		"models.chat: timeouts is missing",
		`models.embed: upstream "d" is not defined`,
		"models.embed: timeouts is missing",
		`models.extract: upstream "b" is not defined`,
		"models.extract: timeouts is missing",
	}, "\n")
	for range 20 {
		if _, err := LoadConfig(path); err == nil || err.Error() != want {
			t.Fatalf("got\n%v\nwant\n%s", err, want)
		}
	}
}
