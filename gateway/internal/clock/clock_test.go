package clock

import "testing"

func TestFakeClockAdvance(t *testing.T) {
	f := NewFake()
	start := f.BootMS()
	f.Advance(250)
	if got := f.BootMS(); got != start+250 {
		t.Fatalf("got %d, want %d", got, start+250)
	}
}

func TestBootMonotonic(t *testing.T) {
	var c Clock = Boot()
	a := c.BootMS()
	if b := c.BootMS(); b < a {
		t.Fatalf("clock went backwards: %d -> %d", a, b)
	}
}
