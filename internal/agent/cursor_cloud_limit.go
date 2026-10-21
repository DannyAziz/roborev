package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// envCursorCloudMaxConcurrent overrides the concurrent launch cap.
	envCursorCloudMaxConcurrent = "ROBOREV_CURSOR_CLOUD_MAX_CONCURRENT"
	// envCursorCloudMaxPerHour overrides the rolling-hour launch cap.
	envCursorCloudMaxPerHour = "ROBOREV_CURSOR_CLOUD_MAX_PER_HOUR"
	// defaultCursorCloudMaxConcurrent matches the default worker count so a
	// normal daemon can run its parallel reviews.
	defaultCursorCloudMaxConcurrent = 4
	// defaultCursorCloudMaxPerHour is one launch a minute, sustained.
	defaultCursorCloudMaxPerHour = 60
)

// cursorCloudLaunchGate counts cursor-cloud launches. A refused acquire
// creates nothing. The concurrent slot is held until release, which the
// review defers until after delete.
type cursorCloudLaunchGate struct {
	mu            sync.Mutex
	now           func() time.Time
	maxConcurrent int
	maxPerHour    int
	active        int
	launched      []time.Time
}

// cursorCloudLaunches is the process-wide cap.
var cursorCloudLaunches = &cursorCloudLaunchGate{}

func (a *CursorCloudAgent) launchGate() *cursorCloudLaunchGate {
	if a != nil && a.launches != nil {
		return a.launches
	}
	return cursorCloudLaunches
}

// CursorCloudCapError is a launch the cap refused before anything was
// sent. The worker requeues the job. RetryAt is when the hourly cap frees a
// slot; it is zero for the concurrent cap, which frees on a release.
type CursorCloudCapError struct {
	Reason  string
	RetryAt time.Time
}

func (e *CursorCloudCapError) Error() string {
	return "cursor-cloud: launch refused: " + e.Reason
}

func (g *cursorCloudLaunchGate) acquire() (func(), func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	concurrent, perHour := g.limits()
	g.prune(now)
	if g.active >= concurrent {
		return nil, nil, &CursorCloudCapError{Reason: fmt.Sprintf("%d agents already running (max concurrent %d)", g.active, concurrent)}
	}
	if len(g.launched) >= perHour {
		return nil, nil, &CursorCloudCapError{
			Reason:  fmt.Sprintf("%d launches in the last hour (max %d per hour)", len(g.launched), perHour),
			RetryAt: g.launched[len(g.launched)-perHour].Add(time.Hour),
		}
	}
	g.active++
	stamp := now
	g.launched = append(g.launched, stamp)
	release := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.active > 0 {
			g.active--
		}
	}
	// refund drops the hourly charge. Call it only when the POST cannot
	// have been sent. A create that may have reached the server keeps the
	// charge so a retry cannot buy a second VM inside the cap.
	refund := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for i, item := range g.launched {
			if item.Equal(stamp) {
				g.launched = append(g.launched[:i], g.launched[i+1:]...)
				return
			}
		}
	}
	return release, refund, nil
}

func (g *cursorCloudLaunchGate) limits() (concurrent, perHour int) {
	concurrent, perHour = g.maxConcurrent, g.maxPerHour
	if concurrent > 0 && perHour > 0 {
		return concurrent, perHour
	}
	defConcurrent, defPerHour := configuredCursorCloudLaunchLimits()
	if concurrent <= 0 {
		concurrent = defConcurrent
	}
	if perHour <= 0 {
		perHour = defPerHour
	}
	return concurrent, perHour
}

func (g *cursorCloudLaunchGate) prune(now time.Time) {
	cutoff := now.Add(-time.Hour)
	kept := g.launched[:0]
	for _, stamp := range g.launched {
		if stamp.After(cutoff) {
			kept = append(kept, stamp)
		}
	}
	g.launched = kept
}

func configuredCursorCloudLaunchLimits() (concurrent, perHour int) {
	return envPositiveInt(envCursorCloudMaxConcurrent, defaultCursorCloudMaxConcurrent),
		envPositiveInt(envCursorCloudMaxPerHour, defaultCursorCloudMaxPerHour)
}

func envPositiveInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}
