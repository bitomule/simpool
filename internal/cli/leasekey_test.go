package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bitomule/simpool/internal/pool"
)

// scratchRepoWithWorktrees builds a real git repository with two linked
// worktrees and returns the three directories a caller can be sitting in.
// Real git, not a stub: what is being pinned here is what `git rev-parse`
// itself answers in a linked worktree, and a stub would pin our belief
// about that instead of the fact.
func scratchRepoWithWorktrees(t *testing.T) (main, wtA, wtB string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	main = filepath.Join(base, "Repo")
	if err := os.MkdirAll(filepath.Join(main, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// A fixed identity and no global config: this must not depend on
		// whose machine it runs on.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	run(main, "init", "-q")
	if err := os.WriteFile(filepath.Join(main, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(main, "add", "f")
	run(main, "commit", "-qm", "init")
	wtA, wtB = filepath.Join(base, "wt-a"), filepath.Join(base, "wt-b")
	run(main, "worktree", "add", "-q", wtA, "-b", "a")
	run(main, "worktree", "add", "-q", wtB, "-b", "b")
	return main, wtA, wtB
}

// keyFrom resolves the default lease key as a caller sitting in dir would
// get it.
func keyFrom(t *testing.T, dir string) string {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	k, err := defaultLeaseKey()
	if err != nil {
		t.Fatalf("defaultLeaseKey in %s: %v", dir, err)
	}
	if err := os.Chdir(prev); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestDefaultLeaseKey_SeparatesWorktreesOfOneRepo is the regression that
// protects the property the whole lease design leans on: the default key
// is the WORKTREE, not the repository.
//
// It is pinned because the plausible-looking change breaks it silently.
// Reaching for the repository instead — `--git-common-dir`, the single
// .git every worktree of a repo shares — would collapse every worktree of
// one repo onto one key, one slot and one simulator, which is the
// contaminated-readings failure reinstated as a default. Nothing about
// the resulting code would look wrong.
func TestDefaultLeaseKey_SeparatesWorktreesOfOneRepo(t *testing.T) {
	t.Setenv(pool.EnvLeaseKey, "")
	main, wtA, wtB := scratchRepoWithWorktrees(t)

	keyMain, keyA, keyB := keyFrom(t, main), keyFrom(t, wtA), keyFrom(t, wtB)

	if keyA == keyB {
		t.Errorf("two worktrees of one repo resolved to one key %q: they would share a simulator", keyA)
	}
	if keyA == keyMain || keyB == keyMain {
		t.Errorf("a linked worktree resolved to the main checkout's key: main=%q a=%q b=%q", keyMain, keyA, keyB)
	}

	// And the other direction, which is what makes the key useful at all:
	// a subdirectory of a worktree is the SAME caller and must resolve to
	// the same key, or a hot loop that happens to run from a package
	// directory would be handed a second simulator.
	if got := keyFrom(t, filepath.Join(main, "pkg")); got != keyMain {
		t.Errorf("a subdirectory resolved to a different key: %q vs the checkout's %q", got, keyMain)
	}
}

// TestDefaultLeaseKey_EnvSeparatesTwoCallersInOneDirectory covers the case
// no path can: two agents running in the SAME directory. Their location is
// genuinely identical, so a path-derived key is identical too, and the
// sticky renewal hands them one simulator before --max is consulted. The
// environment variable is the identity that separates them.
func TestDefaultLeaseKey_EnvSeparatesTwoCallersInOneDirectory(t *testing.T) {
	main, _, _ := scratchRepoWithWorktrees(t)

	t.Setenv(pool.EnvLeaseKey, "")
	shared := keyFrom(t, main)

	t.Setenv(pool.EnvLeaseKey, "agent-one")
	one := keyFrom(t, main)
	t.Setenv(pool.EnvLeaseKey, "agent-two")
	two := keyFrom(t, main)

	if one == two {
		t.Fatalf("two agents in one directory still share the key %q", one)
	}
	if one == shared || two == shared {
		t.Errorf("%s did not override the path-derived key %q", pool.EnvLeaseKey, shared)
	}
	if one != "agent-one" || two != "agent-two" {
		t.Errorf("keys are %q and %q, want the values set in the environment", one, two)
	}
}

// TestDefaultLeaseKey_EnvIsIgnoredWhenBlank keeps the variable from being
// a trap: an exported-but-empty SIMPOOL_LEASE_KEY (which is what a shell
// that sets it conditionally produces) must fall through to the path, not
// make every caller on the machine share the empty key.
func TestDefaultLeaseKey_EnvIsIgnoredWhenBlank(t *testing.T) {
	main, _, _ := scratchRepoWithWorktrees(t)

	t.Setenv(pool.EnvLeaseKey, "")
	want := keyFrom(t, main)
	t.Setenv(pool.EnvLeaseKey, "   ")
	if got := keyFrom(t, main); got != want {
		t.Errorf("a blank %s produced key %q, want the path-derived %q", pool.EnvLeaseKey, got, want)
	}
}
