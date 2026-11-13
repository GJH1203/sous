package fakeupstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Config controls how the fake behaves. Every field can be set by a flag at
// startup or changed at runtime with POST /_config; a change applies to
// requests that arrive after it.
type Config struct {
	// Name is returned in the X-Fake-Upstream header, so a reply can be traced
	// to the instance that served it.
	Name string

	// Chunks is the number of content chunks in a reply. A non-streaming
	// reply waits as long as the stream would have taken, because a real
	// server sends nothing until the whole reply is generated.
	Chunks int
	// ChunkInterval is the pause before each content chunk.
	ChunkInterval time.Duration
	// Reply is the assistant's content, split across the chunks. Empty means
	// "w0 w1 w2 ...", one word per chunk.
	Reply string
	// Dims is the length of each embedding vector.
	Dims int
	// LoadTime is how long after start the fake pretends to be loading its
	// model: until then /health and the model endpoints answer 503.
	LoadTime time.Duration

	// Delay holds back the response headers, and with them the first byte.
	Delay time.Duration
	// Hang reads the request and never answers, until the caller gives up.
	Hang bool
	// FailFirst makes the first N model requests since start or the last
	// reset fail with FailStatus.
	FailFirst int
	// FailRate makes each model request fail with this probability.
	FailRate float64
	// FailStatus is the status code of an injected failure.
	FailStatus int
	// RetryAfter, if positive, adds a Retry-After header (in seconds) to an
	// injected 429 or 503.
	RetryAfter int
	// BreakAfter, if positive, cuts the connection after that many content
	// chunks of a streamed reply, or halfway through a non-streamed body.
	BreakAfter int
	// Seed makes FailRate's choices repeatable. The sequence restarts on reset.
	Seed uint64
}

// DefaultConfig is a healthy fake: five chunks, 100ms apart, no failures.
func DefaultConfig() Config {
	return Config{
		Name:          "fake",
		Chunks:        5,
		ChunkInterval: 100 * time.Millisecond,
		Dims:          8,
		FailStatus:    503,
		Seed:          1,
	}
}

func (c Config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(c.Chunks >= 1, "chunks must be at least 1, got %d", c.Chunks)
	check(c.ChunkInterval >= 0, "chunk_interval must not be negative, got %s", c.ChunkInterval)
	check(c.Dims >= 1 && c.Dims <= 4096, "dims must be between 1 and 4096, got %d", c.Dims)
	check(c.LoadTime >= 0, "load_time must not be negative, got %s", c.LoadTime)
	check(c.Delay >= 0, "delay must not be negative, got %s", c.Delay)
	check(c.FailFirst >= 0, "fail_first must not be negative, got %d", c.FailFirst)
	// Written this way round so that NaN fails too.
	check(c.FailRate >= 0 && c.FailRate <= 1, "fail_rate must be between 0 and 1, got %g", c.FailRate)
	check(c.FailStatus >= 400 && c.FailStatus <= 599, "fail_status must be between 400 and 599, got %d", c.FailStatus)
	check(c.RetryAfter >= 0, "retry_after must not be negative, got %d", c.RetryAfter)
	check(c.BreakAfter >= 0, "break_after must not be negative, got %d", c.BreakAfter)
	return errors.Join(errs...)
}

// configJSON is Config as it appears in /_config. Durations are strings such
// as "250ms", which is what a person types into curl; time.Duration on its
// own would be a number of nanoseconds.
type configJSON struct {
	Name          string  `json:"name"`
	Chunks        int     `json:"chunks"`
	ChunkInterval string  `json:"chunk_interval"`
	Reply         string  `json:"reply"`
	Dims          int     `json:"dims"`
	LoadTime      string  `json:"load_time"`
	Delay         string  `json:"delay"`
	Hang          bool    `json:"hang"`
	FailFirst     int     `json:"fail_first"`
	FailRate      float64 `json:"fail_rate"`
	FailStatus    int     `json:"fail_status"`
	RetryAfter    int     `json:"retry_after"`
	BreakAfter    int     `json:"break_after"`
	Seed          uint64  `json:"seed"`
}

func (c Config) toJSON() configJSON {
	return configJSON{
		Name:          c.Name,
		Chunks:        c.Chunks,
		ChunkInterval: c.ChunkInterval.String(),
		Reply:         c.Reply,
		Dims:          c.Dims,
		LoadTime:      c.LoadTime.String(),
		Delay:         c.Delay.String(),
		Hang:          c.Hang,
		FailFirst:     c.FailFirst,
		FailRate:      c.FailRate,
		FailStatus:    c.FailStatus,
		RetryAfter:    c.RetryAfter,
		BreakAfter:    c.BreakAfter,
		Seed:          c.Seed,
	}
}

func (j configJSON) toConfig() (Config, error) {
	var errs []error
	duration := func(field, s string) time.Duration {
		d, err := time.ParseDuration(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
		return d
	}
	c := Config{
		Name:          j.Name,
		Chunks:        j.Chunks,
		ChunkInterval: duration("chunk_interval", j.ChunkInterval),
		Reply:         j.Reply,
		Dims:          j.Dims,
		LoadTime:      duration("load_time", j.LoadTime),
		Delay:         duration("delay", j.Delay),
		Hang:          j.Hang,
		FailFirst:     j.FailFirst,
		FailRate:      j.FailRate,
		FailStatus:    j.FailStatus,
		RetryAfter:    j.RetryAfter,
		BreakAfter:    j.BreakAfter,
		Seed:          j.Seed,
	}
	return c, errors.Join(errs...)
}

// MarshalJSON writes the config in the /_config format.
func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.toJSON())
}

// UnmarshalJSON merges JSON into the config: fields that are present
// overwrite, absent ones keep their current values. Unknown fields are an
// error, so a typo is not silently ignored.
func (c *Config) UnmarshalJSON(b []byte) error {
	j := c.toJSON()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return err
	}
	merged, err := j.toConfig()
	if err != nil {
		return err
	}
	*c = merged
	return nil
}
