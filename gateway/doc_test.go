package gateway

import "testing"

// TestToolchain proves the module, test runner and race detector are wired
// up before any real code exists. Delete it once the package has real tests.
func TestToolchain(t *testing.T) {
	if got := 1 + 1; got != 2 {
		t.Fatalf("1+1 = %d, want 2", got)
	}
}
