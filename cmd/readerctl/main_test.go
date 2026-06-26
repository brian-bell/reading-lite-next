package main_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderctlBinary_StoreBackedCommandReportsConfigError(t *testing.T) {
	t.Parallel()

	bin := filepath.Join(t.TempDir(), "readerctl")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build error = %v; output:\n%s", err, out)
	}

	cmd := exec.Command(bin, "audit")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("go run unexpectedly succeeded; output:\n%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("go run error = %T %v, want ExitError", err, err)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("exit = %d, want 2; output:\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "store dependency is not configured") {
		t.Fatalf("output = %q, want deterministic config error", string(out))
	}
}
