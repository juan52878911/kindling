package api

import "testing"

func TestParseSignal(t *testing.T) {
	for in, want := range map[string]int{
		"": 15, "SIGTERM": 15, "term": 15, "15": 15, "SIGINT": 2, "QUIT": 3,
		"SIGABRT": 6, "SIGALRM": 14, "SIGPIPE": 13, "SIGPWR": 30, "SIGWINCH": 28,
		"SIGRTMIN": 34, "SIGRTMIN+3": 37, "rtmin+30": 64, "SIGRTMAX": 64, "RTMAX-1": 63,
		"RTMAX-30": 34, "64": 64, "1": 1,
	} {
		got, err := ParseSignal(in)
		if err != nil || got != want {
			t.Errorf("ParseSignal(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "65", "-1", "SIGFOO", "RTMIN+31", "RTMIN-1", "RTMAX+1",
		"RTMAX-31", "RTMIN+", "RTMIN+03", "RTMIN+x", "SIG", "TERM "} {
		if n, err := ParseSignal(in); err == nil {
			t.Errorf("ParseSignal(%q) = %d, want an error", in, n)
		}
	}
}
