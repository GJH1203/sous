package app

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// The app knows the gateway's address and the logical model names, nothing
// more: swapping an upstream or a model must not touch it. These tests hold
// that line for the app's code and config.

// The app's files, relative to the repository root.
var appFiles = []string{"app", "cmd/app", ".env.example"}

func TestAppDoesNotImportTheGateway(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "./app/...", "./cmd/app/...")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "github.com/GJH1203/sous/gateway" || strings.HasPrefix(pkg, "github.com/GJH1203/sous/gateway/") {
			t.Errorf("the app depends on %s", pkg)
		}
	}
}

func TestAppNamesNoUpstreamOrModel(t *testing.T) {
	// What the gateway's configs say about upstreams, which the app must not
	// repeat.
	var forbidden []string
	for _, name := range []string{"gateway.yaml", "gateway.fake.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "config", name))
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Upstreams []struct {
				BaseURL string `yaml:"base_url"`
			} `yaml:"upstreams"`
			Models map[string]struct {
				Model string `yaml:"model"`
			} `yaml:"models"`
		}
		if err := yaml.Load(data, &cfg); err != nil {
			t.Fatal(err)
		}
		for _, u := range cfg.Upstreams {
			// The host and port: a URL can be spelled in more than one way.
			forbidden = append(forbidden, strings.TrimPrefix(strings.TrimPrefix(u.BaseURL, "http://"), "https://"))
		}
		for _, m := range cfg.Models {
			forbidden = append(forbidden, m.Model)
		}
	}
	if len(forbidden) < 6 {
		t.Fatalf("only %d names to look for: %v", len(forbidden), forbidden)
	}

	checked := 0
	for _, root := range appFiles {
		err := filepath.WalkDir(filepath.Join("..", root), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			checked++
			for _, s := range forbidden {
				if strings.Contains(string(data), s) {
					t.Errorf("%s names %q, which belongs in the gateway's config", path, s)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 10 {
		t.Errorf("only %d files checked", checked)
	}
}
