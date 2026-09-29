package tui

import (
	"testing"
	"time"
)

func TestWelcomeGreetingPools(t *testing.T) {
	pools := []struct {
		name      string
		greetings []string
	}{
		{"after midnight", afterMidnightGreetings[:]},
		{"morning", morningGreetings[:]},
		{"afternoon", afternoonGreetings[:]},
		{"evening", eveningGreetings[:]},
	}

	all := make(map[string]string, 200)
	for _, pool := range pools {
		if got := len(pool.greetings); got != 50 {
			t.Errorf("%s pool has %d greetings, want 50", pool.name, got)
		}
		within := make(map[string]struct{}, 50)
		for _, greeting := range pool.greetings {
			if greeting == "" {
				t.Errorf("%s pool contains an empty greeting", pool.name)
			}
			if _, exists := within[greeting]; exists {
				t.Errorf("%s pool contains duplicate %q", pool.name, greeting)
			}
			within[greeting] = struct{}{}
			if other, exists := all[greeting]; exists {
				t.Errorf("greeting %q appears in both %s and %s pools", greeting, other, pool.name)
			}
			all[greeting] = pool.name
		}
	}
	if got := len(all); got != 200 {
		t.Errorf("found %d unique greetings, want 200", got)
	}
}

func TestWelcomeGreetingsBoundaries(t *testing.T) {
	zone := time.FixedZone("test", -7*60*60)
	tests := []struct {
		name string
		hour int
		min  int
		want *[50]string
	}{
		{"05:59", 5, 59, &afterMidnightGreetings},
		{"06:00", 6, 0, &morningGreetings},
		{"11:59", 11, 59, &morningGreetings},
		{"12:00", 12, 0, &afternoonGreetings},
		{"17:59", 17, 59, &afternoonGreetings},
		{"18:00", 18, 0, &eveningGreetings},
		{"23:59", 23, 59, &eveningGreetings},
		{"00:00", 0, 0, &afterMidnightGreetings},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, time.September, 29, tt.hour, tt.min, 0, 0, zone)
			got := welcomeGreetings(now)
			if len(got) != len(tt.want) || &got[0] != &tt.want[0] {
				t.Errorf("welcomeGreetings(%s) selected the wrong pool", tt.name)
			}
		})
	}
}

func TestPickWelcomeGreetingBelongsToExpectedPool(t *testing.T) {
	zone := time.FixedZone("test", 9*60*60)
	times := []time.Time{
		time.Date(2026, time.September, 29, 2, 30, 0, 0, zone),
		time.Date(2026, time.September, 29, 8, 30, 0, 0, zone),
		time.Date(2026, time.September, 29, 14, 30, 0, 0, zone),
		time.Date(2026, time.September, 29, 20, 30, 0, 0, zone),
	}

	for _, now := range times {
		pool := welcomeGreetings(now)
		got := pickWelcomeGreeting(now)
		found := false
		for _, greeting := range pool {
			if got == greeting {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pickWelcomeGreeting(%v) = %q, not in selected pool", now, got)
		}
	}
}
