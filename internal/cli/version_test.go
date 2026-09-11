package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionPrintsStampedVersion(t *testing.T) {
	defer func(old string) { Version = old }(Version)
	Version = "v9.9.9"

	var stdout, stderr bytes.Buffer
	if code := RunVersion(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "simpool v9.9.9\n") {
		t.Errorf("first line should name the version, got %q", out)
	}
	for _, want := range []string{"go      ", "built   "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestVersionShortIsJustTheVersion(t *testing.T) {
	defer func(old string) { Version = old }(Version)
	Version = "v9.9.9"

	var stdout, stderr bytes.Buffer
	if code := RunVersion([]string{"--short"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}
	// The release workflow's Homebrew test compares this against the
	// formula's version, so any extra line here breaks an install.
	if got := stdout.String(); got != "v9.9.9\n" {
		t.Errorf("--short printed %q, want exactly the version and a newline", got)
	}
}

// An unstamped build must never claim a release number. `go build` from a
// checkout fills Main.Version from the nearest reachable tag, so without
// this rule a binary built one commit past v0.16.0 introduces itself as
// v0.16.0 — and this command exists precisely to be trusted when two
// copies of simpool disagree about a pool.
func TestUnstampedCheckoutBuildIsDev(t *testing.T) {
	if got := unstampedVersion(true); got != "dev" {
		t.Errorf("a checkout build reported %q, want \"dev\"", got)
	}
}

func TestVersionRejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunVersion([]string{"--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit %d for an unknown flag, want 2", code)
	}
}
