package console

import (
	"os/exec"
	"testing"
)

// TestConsoleJavaScript runs the unit tests for the console's front-end logic
// (static/lib.js) with Node's built-in test runner. It is skipped where Node
// is not installed; CI runners have it.
func TestConsoleJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping console JavaScript tests")
	}
	out, err := exec.Command(node, "--test", "jstest/*.test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("console JavaScript tests failed:\n%s", out)
	}
}
