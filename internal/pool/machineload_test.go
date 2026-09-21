package pool

import "testing"

// TestParseMachineLoad covers the shapes `sysctl -n vm.loadavg
// vm.swapusage` actually produces, plus the ones a caller must not
// misread. The degraded rows are the point: an unreadable machine has to
// say "unknown", because printing a zero would read as an idle machine and
// that is worse than printing nothing.
func TestParseMachineLoad(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		want  MachineLoad
		print string
	}{
		{
			name:  "both halves, as measured on this machine",
			out:   "{ 12.06 10.32 15.32 }\ntotal = 9216.00M  used = 8007.31M  free = 1208.69M  (encrypted)\n",
			want:  MachineLoad{Load1: 12.06, SwapUsedMB: 8007.31, SwapFreeMB: 1208.69, OK: true},
			print: "load 12.06, swap 1209M free",
		},
		{
			name:  "an idle machine with swap untouched",
			out:   "{ 0.42 0.51 0.60 }\ntotal = 0.00M  used = 0.00M  free = 0.00M  (encrypted)\n",
			want:  MachineLoad{Load1: 0.42, OK: true},
			print: "load 0.42, swap 0M free",
		},
		{
			// A kernel that stops reporting swap must not also cost the
			// load average — the two are parsed independently.
			name:  "swap missing, load still readable",
			out:   "{ 3.00 2.00 1.00 }\n",
			want:  MachineLoad{Load1: 3.00, OK: true},
			print: "load 3.00, swap 0M free",
		},
		{
			name:  "nothing parseable",
			out:   "sysctl: unknown oid\n",
			want:  MachineLoad{},
			print: "load unknown",
		},
		{
			name:  "empty output",
			out:   "",
			want:  MachineLoad{},
			print: "load unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMachineLoad(tc.out)
			if got != tc.want {
				t.Errorf("parseMachineLoad = %+v, want %+v", got, tc.want)
			}
			if s := got.String(); s != tc.print {
				t.Errorf("String() = %q, want %q", s, tc.print)
			}
		})
	}
}

// TestLiveMachineLoad_ReadsThisMachine is the one test that touches the
// real `sysctl`, because a parser that is perfect against captured text and
// wrong about the command it parses would pass everything above.
//
// It asserts only what must be true of ANY machine running this suite —
// the reading succeeded and the load is not negative — rather than a value,
// which would make the verdict depend on what the machine is doing while
// the tests run.
func TestLiveMachineLoad_ReadsThisMachine(t *testing.T) {
	m := liveMachineLoad()
	if !m.OK {
		t.Fatal("could not read load or swap from sysctl on this machine")
	}
	if m.Load1 < 0 {
		t.Errorf("load average is negative: %v", m.Load1)
	}
	if m.String() == "load unknown" {
		t.Errorf("OK reading still prints as unknown: %+v", m)
	}
}

// BenchmarkReadMachineLoad is the answer to "does this make the lease more
// expensive". Reported in the PR against a lease's own measured time.
func BenchmarkReadMachineLoad(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = liveMachineLoad()
	}
}
