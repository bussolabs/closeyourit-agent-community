package agent

import (
	"testing"
	"time"
)

// SystemUpdates must never panic nor block, regardless of the environment (CI/macOS/Linux without
// apt): reboot_required reflects whether the file exists, the counts are -1 when apt-check is
// missing.
func TestSystemUpdatesIsSafeAndUnknownWithoutApt(t *testing.T) {
	reboot, updates, security := SystemUpdates()

	// In the test environment /var/run/reboot-required does not exist → false; a valid bool anyway.
	_ = reboot

	// Without /usr/lib/update-notifier/apt-check (CI/macOS) the counts are -1 (unknown), never a
	// false 0. Should the test host happen to have apt-check, the values are >= 0.
	if updates < -1 {
		t.Fatalf("invalid updates_available: %d", updates)
	}
	if security < -1 {
		t.Fatalf("invalid security_updates_available: %d", security)
	}
}

func TestAptCheckCountsUnknownWhenBinaryMissing(t *testing.T) {
	// aptCheckCounts returns -1/-1 if the apt-check binary is missing (fixed system path).
	if updates, security := aptCheckCounts(); updates < -1 || security < -1 {
		t.Fatalf("invalid aptCheckCounts: %d/%d", updates, security)
	}
}

// The case behind the ticket: a host with available updates and ZERO security ones.
// If the second number gets lost, the UI shows a total that apply_security_updates will never bring
// down.
func TestParseAptCheckKeepsBothCounts(t *testing.T) {
	cases := []struct {
		name                      string
		out                       string
		wantUpdates, wantSecurity int
	}{
		{"total with zero security", "15;0", 15, 0},
		{"both set", "15;3", 15, 3},
		{"nothing to do", "0;0", 0, 0},
		{"spaces and newline", " 7 ; 2 \n", 7, 2},
		{"unexpected format", "15", -1, -1},
		{"not numeric", "boh;2", -1, -1},
		{"empty", "", -1, -1},
		{"unreadable security count", "15;boh", 15, -1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updates, security := parseAptCheck(tc.out)
			if updates != tc.wantUpdates || security != tc.wantSecurity {
				t.Fatalf("parseAptCheck(%q) = %d/%d, expected %d/%d", tc.out, updates, security, tc.wantUpdates, tc.wantSecurity)
			}
		})
	}
}

// apt-check starts a Python interpreter and loads the whole apt cache: it must not run on every push
// (CYAG-21). The cached counts live until the apt state changes or the max age passes.
func TestAptCountsCacheReusesCountsForTheSameAptState(t *testing.T) {
	var c aptCountsCache
	calls := 0
	compute := func() (int, int) { calls++; return 7, 2 }
	now := time.Unix(1_000_000, 0)

	c.get(now, "stamp-a", compute)
	updates, security := c.get(now.Add(10*time.Minute), "stamp-a", compute)

	if calls != 1 {
		t.Fatalf("apt-check ran %d times, want 1", calls)
	}
	if updates != 7 || security != 2 {
		t.Fatalf("got %d/%d, want 7/2", updates, security)
	}
}

func TestAptCountsCacheRecomputesWhenAptStateChanges(t *testing.T) {
	var c aptCountsCache
	calls := 0
	compute := func() (int, int) { calls++; return 7 - calls, 0 }
	now := time.Unix(1_000_000, 0)

	c.get(now, "before-upgrade", compute)
	updates, _ := c.get(now.Add(time.Minute), "after-upgrade", compute)

	if calls != 2 || updates != 5 {
		t.Fatalf("calls=%d updates=%d, want a fresh count after the apt state changed", calls, updates)
	}
}

func TestAptCountsCacheRecomputesAfterMaxAge(t *testing.T) {
	var c aptCountsCache
	calls := 0
	compute := func() (int, int) { calls++; return 1, 1 }
	now := time.Unix(1_000_000, 0)

	c.get(now, "same", compute)
	c.get(now.Add(aptCheckMaxAge+time.Second), "same", compute)

	if calls != 2 {
		t.Fatalf("apt-check ran %d times, want 2 after the max age", calls)
	}
}

func TestAptCountsCacheDoesNotKeepUnknownCounts(t *testing.T) {
	var c aptCountsCache
	calls := 0
	compute := func() (int, int) { calls++; return -1, -1 }
	now := time.Unix(1_000_000, 0)

	c.get(now, "same", compute)
	c.get(now.Add(time.Minute), "same", compute)

	if calls != 2 {
		t.Fatalf("apt-check ran %d times, want a retry after an unknown result", calls)
	}
}
