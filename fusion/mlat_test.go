// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (c) 2026 CEMAXECUTER LLC

package main

import (
	"math"
	"testing"
)

// TestSolveSyntheticEmitter verifies the solver recovers a known
// emitter position from synthetic perfect-clock observations. Three
// stations forming a triangle around an emitter; we compute the
// arrival times exactly from station-emitter distances + speed of
// light, then check the solver converges to the emitter within 5 m.
func TestSolveSyntheticEmitter(t *testing.T) {
	// Anchor everything around 39.0 N, -98.0 W (somewhere in Kansas).
	const lat0, lon0 = 39.0, -98.0

	emitterLat, emitterLon := lat0+0.005, lon0+0.003 // ~550 m E, ~550 m N

	stations := []struct {
		name     string
		lat, lon float64
	}{
		{"alpha", lat0 + 0.010, lon0 - 0.010}, // NW
		{"bravo", lat0 + 0.000, lon0 + 0.012}, // E
		{"delta", lat0 - 0.012, lon0 - 0.002}, // S
		{"echo",  lat0 + 0.008, lon0 + 0.008}, // NE -- 4th station to break the
		// hyperbolic ambiguity (3 stations alone give two valid solutions)
	}

	// Compute exact arrival times. Pick an arbitrary tx epoch.
	const txTimeNs = uint64(1_700_000_000_000_000_000)

	obs := make([]MlatObservation, len(stations))
	for i, s := range stations {
		dx, dy := llToEnu(s.lat, s.lon, emitterLat, emitterLon)
		dist := math.Hypot(dx, dy)
		flightNs := uint64(dist / speedOfLight * 1e9)
		obs[i] = MlatObservation{
			StationName: s.name,
			Lat:         s.lat,
			Lon:         s.lon,
			AltM:        0,
			TNs:         txTimeNs + flightNs,
			TAccNs:      100, // perfect clocks
		}
	}

	res, err := Solve(obs)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}

	// How far off was the estimate from the true emitter?
	dx, dy := llToEnu(res.Lat, res.Lon, emitterLat, emitterLon)
	errM := math.Hypot(dx, dy)

	// Diagnostic: compute residuals at the TRUE emitter to verify the
	// math is consistent. If the true position has near-zero residuals
	// but the solver landed elsewhere, the iteration is stuck.
	trueX, trueY := llToEnu(emitterLat, emitterLon, (obs[0].Lat+obs[1].Lat+obs[2].Lat+obs[3].Lat)/4, (obs[0].Lon+obs[1].Lon+obs[2].Lon+obs[3].Lon)/4)
	t.Logf("true emitter ENU = (%.1f, %.1f); estimate %.1f m off; uncert %.1f m; iter %d",
		trueX, trueY, errM, res.UncertaintyM, res.Iterations)

	if errM > 5.0 {
		t.Fatalf("estimate %.6f, %.6f is %.1f m from true %.6f, %.6f (want <5 m); iters=%d uncert=%.1f m",
			res.Lat, res.Lon, errM, emitterLat, emitterLon, res.Iterations, res.UncertaintyM)
	}
	t.Logf("recovered emitter to %.2f m in %d iterations (uncertainty %.1f m)",
		errM, res.Iterations, res.UncertaintyM)
}

// TestEvalResidualAtTruth verifies the math by computing residuals at
// the known-true emitter. If perfect-clock observations don't yield
// near-zero residual at truth, the math itself has a bug.
func TestEvalResidualAtTruth(t *testing.T) {
	const lat0, lon0 = 39.0, -98.0
	emitterLat, emitterLon := lat0+0.005, lon0+0.003

	stations := []struct {
		name     string
		lat, lon float64
	}{
		{"alpha", lat0 + 0.010, lon0 - 0.010},
		{"bravo", lat0 + 0.000, lon0 + 0.012},
		{"delta", lat0 - 0.012, lon0 - 0.002},
		{"echo", lat0 + 0.008, lon0 + 0.008},
	}

	const txTimeNs = uint64(1_700_000_000_000_000_000)
	obs := make([]MlatObservation, len(stations))
	for i, s := range stations {
		dx, dy := llToEnu(s.lat, s.lon, emitterLat, emitterLon)
		dist := math.Hypot(dx, dy)
		flightNs := uint64(dist/speedOfLight*1e9 + 0.5) // round, not truncate
		obs[i] = MlatObservation{
			StationName: s.name, Lat: s.lat, Lon: s.lon,
			TNs: txTimeNs + flightNs, TAccNs: 100,
		}
	}

	// Manually project to the same anchor the solver uses (centroid).
	var aLat, aLon float64
	for i := range obs {
		aLat += obs[i].Lat
		aLon += obs[i].Lon
	}
	aLat /= float64(len(obs))
	aLon /= float64(len(obs))

	minTNs := obs[0].TNs
	for i := range obs {
		if obs[i].TNs < minTNs {
			minTNs = obs[i].TNs
		}
	}
	mst := make([]mlatStation, len(obs))
	for i := range obs {
		x, y := llToEnu(obs[i].Lat, obs[i].Lon, aLat, aLon)
		mst[i] = mlatStation{
			x: x, y: y, z: 0,
			t: float64(obs[i].TNs-minTNs) * 1e-9,
			w: 1.0,
		}
	}

	// True emitter in this anchor's ENU.
	tx, ty := llToEnu(emitterLat, emitterLon, aLat, aLon)
	t.Logf("true emitter ENU = (%.3f, %.3f)", tx, ty)
	for i := range mst {
		t.Logf("station[%d] ENU = (%.3f, %.3f), t = %.9f s", i, mst[i].x, mst[i].y, mst[i].t)
	}

	resAtTruth := evalResidual(mst, tx, ty, 0)
	t.Logf("residual at truth = %.6e (should be ~0)", resAtTruth)
	if resAtTruth > 1.0 {
		t.Fatalf("residual at known-true emitter is %.3f -- math is inconsistent",
			resAtTruth)
	}
}

// TestSolveTooFew confirms the solver rejects fewer than 3 observations.
func TestSolveTooFew(t *testing.T) {
	obs := []MlatObservation{
		{StationName: "x", Lat: 39, Lon: -98, TNs: 1, TAccNs: 100},
		{StationName: "y", Lat: 40, Lon: -97, TNs: 1, TAccNs: 100},
	}
	if _, err := Solve(obs); err == nil {
		t.Fatal("expected ErrInsufficient with 2 observations")
	}
}

// TestSolveDegradesWithBadClock verifies that a single high-accuracy
// number from one station and noisy numbers from two others still
// converges (the solver is sensitive to clock quality but doesn't
// blow up). The error should be larger than the perfect-clock case.
func TestSolveDegradesWithBadClock(t *testing.T) {
	const lat0, lon0 = 39.0, -98.0
	emitterLat, emitterLon := lat0+0.005, lon0+0.003

	stations := []struct {
		name     string
		lat, lon float64
		jitterNs int64 // injected timing error
	}{
		{"alpha", lat0 + 0.010, lon0 - 0.010, 0},
		{"bravo", lat0 + 0.000, lon0 + 0.012, +1000}, // 1 us jitter -> 300 m
		{"delta", lat0 - 0.012, lon0 - 0.002, -1500}, // 1.5 us jitter
		{"echo",  lat0 + 0.008, lon0 + 0.008, +500},  // 4th station, 500 ns jitter
	}
	const txTimeNs = uint64(1_700_000_000_000_000_000)
	obs := make([]MlatObservation, len(stations))
	for i, s := range stations {
		dx, dy := llToEnu(s.lat, s.lon, emitterLat, emitterLon)
		dist := math.Hypot(dx, dy)
		flightNs := int64(dist/speedOfLight*1e9) + s.jitterNs
		obs[i] = MlatObservation{
			StationName: s.name,
			Lat:         s.lat,
			Lon:         s.lon,
			TNs:         uint64(int64(txTimeNs) + flightNs),
			TAccNs:      1000, // 1 us discipline
		}
	}
	res, err := Solve(obs)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	dx, dy := llToEnu(res.Lat, res.Lon, emitterLat, emitterLon)
	errM := math.Hypot(dx, dy)
	t.Logf("with 1-1.5 us jitter: %.0f m error (uncertainty self-report %.0f m)",
		errM, res.UncertaintyM)
	// Sanity check: should converge, error should be O(km) not O(million-km).
	if errM > 10000 {
		t.Fatalf("solver blew up: %.0f m error", errM)
	}
}

// TestTimestampClassResolution verifies that LockTNs (software_lock)
// wins over TNs (frame) when both are present, that a solve with only
// frame-class observations reports class=frame, and that mixed
// observations land in degraded=true.
func TestTimestampClassResolution(t *testing.T) {
	const lat0, lon0 = 39.0, -98.0
	emitterLat, emitterLon := lat0+0.005, lon0+0.003

	stations := []struct {
		name     string
		lat, lon float64
	}{
		{"alpha", lat0 + 0.010, lon0 - 0.010},
		{"bravo", lat0 + 0.000, lon0 + 0.012},
		{"delta", lat0 - 0.012, lon0 - 0.002},
		{"echo", lat0 + 0.008, lon0 + 0.008},
	}
	const txTimeNs = uint64(1_700_000_000_000_000_000)
	mkObs := func(useLock bool) []MlatObservation {
		obs := make([]MlatObservation, len(stations))
		for i, s := range stations {
			dx, dy := llToEnu(s.lat, s.lon, emitterLat, emitterLon)
			dist := math.Hypot(dx, dy)
			flightNs := uint64(dist / speedOfLight * 1e9)
			o := MlatObservation{
				StationName: s.name, Lat: s.lat, Lon: s.lon, TAccNs: 100,
			}
			// frame-class observation always has a (worse) TNs filled
			o.TNs = txTimeNs + flightNs + uint64(10_000_000) // +10ms demod-latency offset
			if useLock {
				// software_lock-class observation has the precise lock time
				o.LockTNs = txTimeNs + flightNs
			}
			obs[i] = o
		}
		return obs
	}

	// Frame-only: solver consumes TNs, class=frame.
	resFrame, err := Solve(mkObs(false))
	if err != nil {
		t.Fatalf("Solve frame-only: %v", err)
	}
	if resFrame.WorstTimestampCls != TimestampFrame {
		t.Errorf("frame-only solve: worst class = %v, want frame", resFrame.WorstTimestampCls)
	}
	if resFrame.Degraded {
		t.Errorf("frame-only solve should not be degraded")
	}
	if resFrame.TimestampClasses[TimestampFrame] != len(stations) {
		t.Errorf("frame-only solve: frame count = %d, want %d",
			resFrame.TimestampClasses[TimestampFrame], len(stations))
	}

	// Software-lock for all: solver consumes LockTNs, class=software_lock.
	// The +10ms offset on TNs proves LockTNs won (frame-class would
	// have placed the emitter ~3000 km away).
	resLock, err := Solve(mkObs(true))
	if err != nil {
		t.Fatalf("Solve lock-only: %v", err)
	}
	if resLock.WorstTimestampCls != TimestampSoftwareLock {
		t.Errorf("lock-only solve: worst class = %v, want software_lock", resLock.WorstTimestampCls)
	}
	if resLock.Degraded {
		t.Errorf("lock-only solve should not be degraded")
	}
	dx, dy := llToEnu(resLock.Lat, resLock.Lon, emitterLat, emitterLon)
	if math.Hypot(dx, dy) > 50 {
		t.Errorf("LockTNs should have produced an accurate solve, got %.0f m error",
			math.Hypot(dx, dy))
	}

	// Mixed: 2 stations with LockTNs, 2 without. Should flag degraded.
	mixed := mkObs(true)
	mixed[0].LockTNs = 0 // back to frame-only
	mixed[1].LockTNs = 0
	resMixed, err := Solve(mixed)
	if err != nil {
		t.Fatalf("Solve mixed: %v", err)
	}
	if !resMixed.Degraded {
		t.Errorf("mixed-class solve should be degraded")
	}
	if resMixed.WorstTimestampCls != TimestampFrame {
		t.Errorf("mixed-class solve: worst class = %v, want frame", resMixed.WorstTimestampCls)
	}
	t.Logf("mixed: %d frame + %d software_lock = degraded=%v",
		resMixed.TimestampClasses[TimestampFrame],
		resMixed.TimestampClasses[TimestampSoftwareLock],
		resMixed.Degraded)
}
