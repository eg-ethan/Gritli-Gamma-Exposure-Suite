// US regular-session boundary — the v1 staleness-gate and β-refresh clock
// (architecture.md §12.6, §12.7). Hardcoded 09:30–16:00 America/New_York: the
// suite trades US products only, and the real trading calendar stays deferred
// with the GARCH one. The serve binary imports _ "time/tzdata" so
// America/New_York resolves on Windows and static Linux builds; without tzdata
// LoadLocation fails and the gate would silently never arm.
package hedge

import (
	"sync"
	"time"
)

var etOnce sync.Once
var etLoc *time.Location
var etErr error

func eastern() (*time.Location, error) {
	etOnce.Do(func() {
		etLoc, etErr = time.LoadLocation("America/New_York")
	})
	return etLoc, etErr
}

// USSession reduces now to (sessionKey, regularHours): the ET calendar day
// yyyyMMdd and whether the moment sits inside 09:30–16:00 ET regular hours.
// sessionKey is the lazy β-refresh trigger — a US session spans two UTC days,
// so "first tick of a new session" must key on the ET date, not UTC midnight.
// On tzdata failure it reports the UTC key and regularHours=false (the gate
// disarms rather than false-tripping overnight) — the serve binary's tzdata
// import makes that path unreachable in practice.
func USSession(now time.Time) (sessionKey string, regularHours bool) {
	loc, err := eastern()
	if err != nil || loc == nil {
		return now.UTC().Format("20060102") + "Z", false
	}
	et := now.In(loc)
	key := et.Format("20060102")
	h, m := et.Hour(), et.Minute()
	mins := h*60 + m
	return key, mins >= 9*60+30 && mins < 16*60
}
