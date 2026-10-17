package loopguard

import "testing"

func TestStepCeiling(t *testing.T) {
	g := New(3)
	for i := 0; i < 3; i++ {
		if sig, _ := g.Tick(); sig != SignalOK {
			t.Fatalf("step %d should be allowed", i)
		}
	}
	if sig, _ := g.Tick(); sig != SignalStop {
		t.Fatal("4th tick over maxSteps=3 must hard-stop")
	}
}

func TestRepeatedIdenticalCalls(t *testing.T) {
	g := New(30)
	sig1 := Signature("web_fetch", `{"url":"http://a.com"}`)
	sig2 := Signature("web_fetch", `{ "url": "http://a.com" }`) // whitespace-normalized
	if sig1 != sig2 {
		t.Fatal("argument whitespace must normalize to the same signature")
	}
	sigWarn := SignalOK
	sigStop := SignalOK
	// First call establishes the baseline; the next RepeatSameCall identical
	// calls are the actual repeats, so the fuse trips on call #(RepeatSameCall+1).
	for i := 0; i < RepeatSameCall+1; i++ {
		sig, _ := g.Observe("web_fetch", sig1, "page", false)
		if sig == SignalWarn {
			sigWarn = sig
		}
		if sig == SignalStop {
			sigStop = sig
		}
	}
	if sigWarn != SignalWarn {
		t.Fatal("expected a warn before the hard stop")
	}
	if sigStop != SignalStop {
		t.Fatal("repeated identical calls must hard-stop")
	}
}

func TestDifferentCallsDoNotTrip(t *testing.T) {
	g := New(30)
	for i := 0; i < 10; i++ {
		sig := Signature("web_search", `{"q":"query`+string(rune('a'+i))+`"}`)
		if sig, _ := g.Observe("web_search", sig, "result "+string(rune('a'+i)), false); sig != SignalOK {
			t.Fatalf("distinct calls must not trip, got %s at %d", sig, i)
		}
	}
}

func TestConsecutiveFailures(t *testing.T) {
	g := New(30)
	sig := Signature("shell_exec", `{"command":"x"}`)
	var last string
	for i := 0; i < MaxFailStreak; i++ {
		last, _ = g.Observe("shell_exec", sig, "error: not found", true)
	}
	if last != SignalStop {
		t.Fatalf("consecutive failures must hard-stop, got %q", last)
	}
}

func TestSameResultNoProgress(t *testing.T) {
	g := New(30)
	var last string
	for i := 0; i < MaxSameResult+1; i++ {
		// Different arguments, but the page returns the same empty body.
		sig := Signature("web_fetch", `{"url":"http://x.com/`+string(rune('a'+i))+`"}`)
		last, _ = g.Observe("web_fetch", sig, "", false)
	}
	// Empty results normalize to no key, so this should NOT trip — empty is
	// treated as no data rather than a repeated stall signal.
	if last == SignalStop {
		t.Fatal("empty results must not count as repeated identical output")
	}
	// Non-empty identical results should trip.
	g2 := New(30)
	last = ""
	for i := 0; i < MaxSameResult+1; i++ {
		sig := Signature("web_fetch", `{"url":"http://x.com/`+string(rune('a'+i))+`"}`)
		last, _ = g2.Observe("web_fetch", sig, "Access Denied", false)
	}
	if last != SignalStop {
		t.Fatalf("identical non-empty results must hard-stop, got %q", last)
	}
}

func TestRecoveryResetsFailures(t *testing.T) {
	g := New(30)
	sig := Signature("shell_exec", `{"command":"x"}`)
	for i := 0; i < MaxFailStreak-1; i++ {
		if s, _ := g.Observe("shell_exec", sig, "error", true); s == SignalStop {
			t.Fatal("too early to stop")
		}
	}
	// One success with fresh output breaks the streak.
	g.Observe("shell_exec", Signature("shell_exec", `{"command":"y"}`), "ok output", false)
	if g.failStreak != 0 {
		t.Fatal("successful call should reset the failure streak")
	}
}
