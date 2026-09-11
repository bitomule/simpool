package cli

import (
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Version is the release this binary was built for, stamped at link time
// by the release workflow (-ldflags "-X ...cli.Version=$TAG"). An
// unstamped build deliberately reports "dev" rather than inventing a
// number: claiming to be a release it is not is worse than admitting it
// does not know, since the whole point of this command is to be trusted
// when two copies of simpool disagree about a pool.
var Version = ""

// RunVersion prints what this binary is, for the case a pool is shared by
// copies that are not the same version. That case is not hypothetical:
// since v0.17.0 a slim-provisioning acquirer resolves a default --max of
// 6 while an older reaper on the same pool resolves 3, and would evict
// slots the acquirer had every right to create. Diagnosing that starts
// with asking each side what it is, and until now the only way to ask was
// the package manager — which knows what it installed, not what is on
// PATH.
func RunVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	short := fs.Bool("short", false, "print only the version, with no build details")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	version := Version
	revision, modified := buildRevision()
	if version == "" {
		version = unstampedVersion(revision != "")
	}

	if *short {
		fmt.Fprintln(stdout, version)
		return 0
	}

	fmt.Fprintf(stdout, "simpool %s\n", version)
	if revision != "" {
		dirty := ""
		if modified {
			dirty = " (working tree modified)"
		}
		fmt.Fprintf(stdout, "commit  %s%s\n", revision, dirty)
	}
	fmt.Fprintf(stdout, "go      %s\n", runtime.Version())
	fmt.Fprintf(stdout, "built   %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return 0
}

// unstampedVersion decides what to call a binary the release workflow did
// not stamp, given whether it was built from a VCS checkout.
//
// The subtlety worth writing down, because it produced a wrong answer
// before it was handled: `go build` from a checkout fills Main.Version
// with a version derived from the nearest reachable tag, so a build of
// unreleased work sitting one commit past v0.16.0 cheerfully calls itself
// "v0.16.0". For a command whose entire job is to be believed when two
// copies disagree, that is worse than useless. A checkout build is
// therefore always "dev" — its commit is printed right underneath, which
// is the honest and more useful identifier anyway.
//
// Main.Version is only trusted when there is no VCS stamp at all, which
// is the `go install module@version` case: there the version really is
// the module version the user asked for, and no checkout exists to
// misread.
func unstampedVersion(fromCheckout bool) string {
	if fromCheckout {
		return "dev"
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// buildRevision digs the VCS stamp out of build info. `go build` records
// it automatically from a git checkout; a build from a tarball or with
// -buildvcs=false has none, which is reported as absent rather than
// guessed at.
func buildRevision() (revision string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = strings.EqualFold(setting.Value, "true")
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision, modified
}
