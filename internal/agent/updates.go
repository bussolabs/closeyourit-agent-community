package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SystemUpdates reports the host update state: whether a reboot is needed (kernel/libc upgraded),
// how many packages can be installed in total and how many of those are SECURITY updates.
// Debian/Ubuntu: `/var/run/reboot-required` + update-notifier `apt-check`. Both counts are -1 when
// the data is not available (non-apt distro or missing apt-check) → the server treats it as
// "unknown", not as "0 updates".
//
// The two numbers are kept apart because they measure different things: `apply_security_updates`
// installs ONLY the security origins, so a host with 15 total updates and 0 security ones will never
// see the total go down with that action (it needs `apply_all_updates`). Reporting only the total,
// the UI has no way to explain it and the action looks broken while it is working correctly.
func SystemUpdates() (rebootRequired bool, updatesAvailable, securityUpdatesAvailable int) {
	_, err := os.Stat("/var/run/reboot-required")
	rebootRequired = err == nil
	updatesAvailable, securityUpdatesAvailable = aptCounts.get(time.Now(), aptStateStamp(), aptCheckCounts)
	return rebootRequired, updatesAvailable, securityUpdatesAvailable
}

// aptCheckMaxAge bounds how long cached counts live when the apt state looks unchanged.
const aptCheckMaxAge = time.Hour

// aptStatePaths change when packages are installed (dpkg status) or the lists are refreshed.
var aptStatePaths = []string{"/var/lib/dpkg/status", "/var/lib/apt/lists"}

var aptCounts aptCountsCache

// aptCountsCache keeps the apt-check counts between pushes: apt-check starts Python and loads the
// whole apt cache, too heavy for every tick (CYAG-21). An upgrade changes the apt state stamp, so
// the counts refresh on the next push and an applied update is visible at once.
type aptCountsCache struct {
	mu                sync.Mutex
	stamp             string
	at                time.Time
	updates, security int
	valid             bool
}

func (c *aptCountsCache) get(now time.Time, stamp string, compute func() (int, int)) (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.stamp == stamp && now.Sub(c.at) < aptCheckMaxAge {
		return c.updates, c.security
	}
	updates, security := compute()
	// An unknown result (timeout, missing binary) is not kept: the next push tries again.
	c.valid = updates >= 0
	c.stamp, c.at, c.updates, c.security = stamp, now, updates, security
	return updates, security
}

// aptStateStamp joins the modification times of the apt state paths; a missing path counts as 0.
func aptStateStamp() string {
	var b strings.Builder
	for _, path := range aptStatePaths {
		if info, err := os.Stat(path); err == nil {
			b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		}
		b.WriteByte(';')
	}
	return b.String()
}

// apt-check prints "updates;security_updates" (on stderr). Defensive timeout: it only reads the apt
// cache (no network), but it must never block the push loop. An unexpected format degrades to
// "unknown" on both counts: better no data than made-up data.
func aptCheckCounts() (updates, security int) {
	const path = "/usr/lib/update-notifier/apt-check"
	if _, err := os.Stat(path); err != nil {
		return -1, -1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path).CombinedOutput()
	if err != nil {
		return -1, -1
	}

	return parseAptCheck(string(out))
}

// parseAptCheck reads apt-check's "total;security" format. Kept apart from execution because it is
// the part that decides what the user sees: this is where the second number used to be dropped.
func parseAptCheck(out string) (updates, security int) {
	fields := strings.Split(strings.TrimSpace(out), ";")
	if len(fields) < 2 {
		return -1, -1
	}
	updates, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return -1, -1
	}
	security, err = strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return updates, -1
	}
	return updates, security
}
