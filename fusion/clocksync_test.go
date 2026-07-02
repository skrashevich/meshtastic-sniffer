// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (c) 2026 CEMAXECUTER LLC

package main

import (
	"math"
	"testing"
	"time"
)

// helper: build a synthetic 3-station scene around a known anchor.
type scene struct {
	stations []stationCoord
	anchor   AnchorNode
	cs       *ClockSync
}

func newScene(t *testing.T, anchorAccuracyNs float64) scene {
	t.Helper()
	const lat0, lon0 = 39.0, -98.0
	stations := []stationCoord{
		{"alpha", lat0 + 0.010, lon0 - 0.010},
		{"bravo", lat0 + 0.000, lon0 + 0.012},
		{"delta", lat0 - 0.012, lon0 - 0.002},
	}
	anchor := AnchorNode{
		NodeID: "!cafe1234", Lat: lat0 + 0.003, Lon: lon0 + 0.001,
		Type: AnchorDeclared, AccuracyNs: anchorAccuracyNs,
	}
	cfg := DefaultClockSyncConfig()
	cfg.MinAnchorEvents = 5 // smaller for faster tests
	cs := NewClockSync(cfg)
	if err := cs.AddAnchor(anchor); err != nil {
		t.Fatalf("AddAnchor: %v", err)
	}
	return scene{stations: stations, anchor: anchor, cs: cs}
}

// mkAnchorCluster builds a Cluster as the fusion event loop would
// have built one when all three stations hear the same anchor packet.
// stationOffsetNs is added to each station's local clock so the
// "expected_dt" math has something to recover.
func (s *scene) mkAnchorCluster(packetID uint32, txTimeNs uint64,
	offsetByStation map[string]float64) *Cluster {
	c := &Cluster{
		Frame: Frame{From: s.anchor.NodeID, PacketID: packetID},
	}
	for _, st := range s.stations {
		// True propagation: anchor -> station distance / c, in ns.
		dM := distMeters(s.anchor.Lat, s.anchor.Lon, st.Lat, st.Lon)
		propNs := uint64(dM / speedOfLight * 1e9)
		lockTNs := txTimeNs + propNs + uint64(offsetByStation[st.Name])
		c.Observations = append(c.Observations, Observation{
			Station: st.Name, StationLat: st.Lat, StationLon: st.Lon,
			PreambleLockTNs: lockTNs,
			RssiDB:          -90.0, // weak enough to clear the RSSI gate
		})
	}
	return c
}

// TestPairOffsetConvergence: feed the same anchor packet at increasing
// tx times with constant per-station offsets. Median per-pair offset
// should converge to the injected offset; status goes warming->converged.
func TestPairOffsetConvergence(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{
		"alpha": 0, // reference (smallest name lex)
		"bravo": 50_000.0, // 50 us late
		"delta": -30_000.0, // 30 us early
	}
	for i := 0; i < 12; i++ {
		c := s.mkAnchorCluster(uint32(1000+i), uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	// Pair (alpha, bravo): pair stored canonically as alpha|bravo
	// (lex order), median should be bravo - alpha = +50000 ns.
	po := s.cs.PairSnapshotByStations("alpha", "bravo")
	if po == nil {
		t.Fatal("alpha|bravo pair missing")
	}
	if po.Status != "converged" {
		t.Errorf("alpha|bravo status=%s, want converged", po.Status)
	}
	if math.Abs(po.MedianNs-50000.0) > 50.0 {
		t.Errorf("alpha|bravo median=%.1f ns, want ~50000", po.MedianNs)
	}
	// Pair (alpha, delta): delta - alpha = -30000.
	po = s.cs.PairSnapshotByStations("alpha", "delta")
	if po == nil {
		t.Fatal("alpha|delta pair missing")
	}
	if po.Status != "converged" {
		t.Errorf("alpha|delta status=%s, want converged", po.Status)
	}
	if math.Abs(po.MedianNs - -30000.0) > 50.0 {
		t.Errorf("alpha|delta median=%.1f ns, want ~-30000", po.MedianNs)
	}
}

// TestSpoofedPositionRejected: an observation whose from-id is NOT in
// the anchor registry must not update the pair graph -- even if the
// packet claims a position (POSITION_APP). We exercise the negative
// path: feed a non-anchor cluster, verify pair state is untouched.
func TestSpoofedPositionRejected(t *testing.T) {
	s := newScene(t, 0)
	// Spoofed cluster: same shape as anchor traffic, but from-id is
	// some random Meshtastic node that operator never declared.
	spoof := &Cluster{Frame: Frame{From: "!deadbeef", PacketID: 42}}
	for _, st := range s.stations {
		spoof.Observations = append(spoof.Observations, Observation{
			Station: st.Name, StationLat: st.Lat, StationLon: st.Lon,
			PreambleLockTNs: 1_700_000_000_000_000_000,
			RssiDB:          -90.0,
		})
	}
	for i := 0; i < 20; i++ {
		spoof.Frame.PacketID = uint32(i)
		s.cs.FeedCluster(spoof)
	}
	if got := s.cs.Snapshot().PairsKnown; got != 0 {
		t.Errorf("non-anchor traffic produced %d pairs, want 0", got)
	}
	if po := s.cs.PairSnapshotByStations("alpha", "bravo"); po != nil {
		t.Errorf("alpha|bravo got created from spoofed traffic: %+v", po)
	}
}

// TestSyncClassGating: CorrectAndClassify returns software_lock until
// the pair has converged. After enough anchor observations it returns
// sync.
func TestSyncClassGating(t *testing.T) {
	s := newScene(t, 0)
	// First three packets should leave pairs warming, class=software_lock.
	for i := 0; i < 3; i++ {
		c := s.mkAnchorCluster(uint32(i), uint64(1_700_000_000_000_000_000+i*1_000_000),
			map[string]float64{"alpha": 0, "bravo": 1000, "delta": -2000})
		s.cs.FeedCluster(c)
	}
	obs := Observation{Station: "bravo", PreambleLockTNs: 1_700_000_000_000_000_000}
	_, cls := s.cs.CorrectAndClassify(obs, "alpha")
	if cls != TimestampSoftwareLock {
		t.Errorf("after 3 obs, class=%v, want software_lock", cls)
	}
	// Push past MinAnchorEvents.
	for i := 3; i < 12; i++ {
		c := s.mkAnchorCluster(uint32(i), uint64(1_700_000_000_000_000_000+i*1_000_000),
			map[string]float64{"alpha": 0, "bravo": 1000, "delta": -2000})
		s.cs.FeedCluster(c)
	}
	correctedTNs, cls := s.cs.CorrectAndClassify(obs, "alpha")
	if cls != TimestampSync {
		t.Errorf("after 12 obs, class=%v, want sync", cls)
	}
	// Correction should subtract ~1000 ns to align bravo onto alpha's
	// reference clock (bravo runs +1000 ns offset).
	delta := int64(correctedTNs) - int64(obs.PreambleLockTNs)
	if math.Abs(float64(delta) - -1000.0) > 50.0 {
		t.Errorf("bravo correction delta=%d, want ~-1000", delta)
	}
}

// TestAnchorRemovedExpires: when no new anchor packets arrive for
// MaxAgeS, samples age out and the pair status moves to stale.
// We accelerate this by setting a tiny MaxAgeS.
func TestAnchorRemovedExpires(t *testing.T) {
	s := newScene(t, 0)
	// Saturate to converged.
	for i := 0; i < 12; i++ {
		c := s.mkAnchorCluster(uint32(i), uint64(1_700_000_000_000_000_000+i*1_000_000),
			map[string]float64{"alpha": 0, "bravo": 50_000, "delta": -30_000})
		s.cs.FeedCluster(c)
	}
	po := s.cs.PairSnapshotByStations("alpha", "bravo")
	if po == nil || po.Status != "converged" {
		t.Fatalf("expected converged pair, got %+v", po)
	}
	// Force age-out: rewind LastUpdate beyond MaxAgeS, then exercise
	// the status recomputation by feeding any cluster (no-op for
	// non-anchor traffic but triggers no recompute either). Cleaner:
	// reach in via mu and bump status directly.
	s.cs.mu.Lock()
	for _, pp := range s.cs.pairs {
		pp.LastUpdate = time.Now().Add(-time.Duration(s.cs.config.MaxAgeS+1) * time.Second)
		pp.updateStatus(s.cs.config)
	}
	s.cs.mu.Unlock()
	po = s.cs.PairSnapshotByStations("alpha", "bravo")
	if po == nil || po.Status != "stale" {
		t.Errorf("after age-out, status=%v, want stale", po.Status)
	}
}

// TestRSSIGate: observations with implausibly-strong RSSI (above the
// gate) must not contribute to pair offsets. They DO bump the gated
// counter so operators can see "your anchor is saturating sniffer X."
func TestRSSIGate(t *testing.T) {
	s := newScene(t, 0)
	// Build an anchor cluster where one station's RSSI is hot (too
	// strong -> indicates near-field). The other two are normal.
	c := s.mkAnchorCluster(99, 1_700_000_000_000_000_000,
		map[string]float64{"alpha": 0, "bravo": 0, "delta": 0})
	// Saturate alpha's RSSI.
	c.Observations[0].RssiDB = -5.0 // above default gate -20 dBm
	s.cs.FeedCluster(c)
	if got := s.cs.Snapshot().ObservationsRSSIGated; got != 1 {
		t.Errorf("ObservationsRSSIGated=%d, want 1", got)
	}
	// Pair (alpha,*) should not exist; pair (bravo,delta) should.
	if po := s.cs.PairSnapshotByStations("alpha", "bravo"); po != nil {
		t.Errorf("alpha|bravo should not have been created (alpha gated): %+v", po)
	}
	if po := s.cs.PairSnapshotByStations("bravo", "delta"); po == nil {
		t.Errorf("bravo|delta missing despite normal RSSI on both")
	}
}

// TestCheckAnchorPlacement: a declared anchor within MinDistanceM of
// any sniffer station logs a warning naming both.
func TestCheckAnchorPlacement(t *testing.T) {
	cfg := DefaultClockSyncConfig()
	cfg.MinDistanceM = 50.0
	cs := NewClockSync(cfg)
	const lat0, lon0 = 39.0, -98.0
	// Anchor ~5 m from station 'rooftop' (way under threshold).
	if err := cs.AddAnchor(AnchorNode{
		NodeID: "!nearby", Lat: lat0, Lon: lon0, Type: AnchorDeclared,
	}); err != nil {
		t.Fatal(err)
	}
	dLat := 5.0 / 6371000.0 * 180.0 / math.Pi
	stations := []stationCoord{
		{"rooftop", lat0 + dLat, lon0},                        // ~5 m away
		{"far_friend", lat0 + 0.005, lon0 + 0.005},            // ~700 m away (OK)
	}
	warns := cs.CheckAnchorPlacement(stations)
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(warns), warns)
	}
	w := warns[0]
	if w.Code != "anchor_too_close" {
		t.Errorf("warning code = %q, want anchor_too_close", w.Code)
	}
	if w.AnchorID != "!nearby" || w.StationName != "rooftop" {
		t.Errorf("warning ids = (%s, %s), want (!nearby, rooftop)", w.AnchorID, w.StationName)
	}
	if w.DistanceM > 10 || w.MinM != 50 {
		t.Errorf("warning distances unexpected: DistanceM=%.2f MinM=%.2f", w.DistanceM, w.MinM)
	}
	if !containsAll(w.Message, "!nearby", "rooftop") {
		t.Errorf("warning message missing names: %q", w.Message)
	}

	// Retain via SetAnchorWarnings; AnchorWarnings returns a copy.
	cs.SetAnchorWarnings(warns)
	got := cs.AnchorWarnings()
	if len(got) != 1 || got[0].AnchorID != "!nearby" {
		t.Errorf("AnchorWarnings round-trip failed: %+v", got)
	}
	// Mutating the returned slice must not corrupt internal state.
	got[0].AnchorID = "mutated"
	if cs.AnchorWarnings()[0].AnchorID != "!nearby" {
		t.Errorf("AnchorWarnings returned mutable internal slice")
	}

	// Empty list returns non-nil empty slice, not nil.
	cs.SetAnchorWarnings(nil)
	if got := cs.AnchorWarnings(); got == nil || len(got) != 0 {
		t.Errorf("AnchorWarnings after Set(nil) = %v, want empty non-nil slice", got)
	}
}

// TestAnchorWarnings_NilReceiver exercises the nil-safe path: a fusion
// process with --clock-sync=off has globalClockSync == nil but the HTTP
// handler still calls AnchorWarnings(). Must not panic.
func TestAnchorWarnings_NilReceiver(t *testing.T) {
	var cs *ClockSync
	cs.SetAnchorWarnings([]ClockSyncWarning{{Code: "x"}}) // must not panic
	if got := cs.AnchorWarnings(); got != nil {
		t.Errorf("nil receiver AnchorWarnings = %v, want nil", got)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if idx := indexOf(s, sub); idx < 0 {
			return false
		}
	}
	return true
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestAnchorParseCLI exercises the --calibration-node parser.
func TestAnchorParseCLI(t *testing.T) {
	a, err := parseAnchorCLI("!abcd1234:lat=39.0:lon=-98.0:alt_m=100:accuracy_ns=500")
	if err != nil {
		t.Fatal(err)
	}
	if a.NodeID != "!abcd1234" || a.Lat != 39.0 || a.Lon != -98.0 ||
		a.AltM != 100 || a.AccuracyNs != 500 {
		t.Errorf("bad parse: %+v", a)
	}
	// Missing lat/lon is an error.
	if _, err := parseAnchorCLI("!nolatlon:alt=10"); err == nil {
		t.Error("missing lat/lon should error")
	}
	// Unknown key.
	if _, err := parseAnchorCLI("!foo:lat=1:lon=2:bogus=3"); err == nil {
		t.Error("unknown key should error")
	}
}

// Clock-monotonicity guard tests (0d).
//
// The guard exists because volunteer stations on NTP/host clocks can step
// backward (chrony correction, suspend-resume, VM jump). Without the
// guard, a backward step feeds a giant negative sample into PairOffset
// that poisons the median window for up to MaxAgeS=600s. The fix:
// detect per-station monotonicity violations during FeedCluster, drop
// the offending observation, delete pair state touching that station so
// future anchor observations retrain from scratch under the new clock
// baseline, and emit a ClockSyncWarning visible via the existing
// /api/clock-sync/warnings endpoint.

// 1. Monotonic anchor observations converge normally; the guard never
//    trips.
func TestMonotonicity_NormalConvergesNoSteps(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{"alpha": 0, "bravo": 50_000.0, "delta": -30_000.0}
	for i := 0; i < 8; i++ {
		c := s.mkAnchorCluster(uint32(1000+i),
			uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	st := s.cs.Snapshot()
	if st.ClockStepsDetected != 0 || st.ClockStepsResetPairs != 0 {
		t.Errorf("monotonic feed tripped guard: detected=%d reset=%d",
			st.ClockStepsDetected, st.ClockStepsResetPairs)
	}
	if po := s.cs.PairSnapshotByStations("alpha", "bravo"); po == nil || po.Status != "converged" {
		t.Errorf("alpha|bravo did not converge under monotonic feed: %+v", po)
	}
}

// 2. A backward step beyond tolerance resets pairs touching that
//    station; pairs NOT touching it survive.
func TestMonotonicity_BackwardStepResetsPairsTouchingStation(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{"alpha": 0, "bravo": 50_000.0, "delta": -30_000.0}
	// Warm up: 8 monotonic clusters; every pair should be converged.
	for i := 0; i < 8; i++ {
		c := s.mkAnchorCluster(uint32(1000+i),
			uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	if s.cs.PairSnapshotByStations("alpha", "bravo") == nil ||
		s.cs.PairSnapshotByStations("alpha", "delta") == nil ||
		s.cs.PairSnapshotByStations("bravo", "delta") == nil {
		t.Fatal("warm-up: all three pairs should exist after 8 monotonic feeds")
	}
	// Now feed a cluster where alpha's preamble_lock_t_ns steps backward
	// by ~1 second relative to its prior value (a typical NTP step).
	// txTimeNs continues to advance normally; only alpha's offset jumps.
	stepCluster := s.mkAnchorCluster(2000,
		uint64(1_700_000_000_000_000_000+9*1_000_000),
		map[string]float64{"alpha": -1_000_000_000.0, "bravo": 50_000.0, "delta": -30_000.0})
	s.cs.FeedCluster(stepCluster)

	st := s.cs.Snapshot()
	if st.ClockStepsDetected != 1 {
		t.Errorf("ClockStepsDetected=%d, want 1", st.ClockStepsDetected)
	}
	// Two pairs touch alpha (alpha|bravo, alpha|delta); both must be gone.
	if po := s.cs.PairSnapshotByStations("alpha", "bravo"); po != nil {
		t.Errorf("alpha|bravo should be deleted by step; got %+v", po)
	}
	if po := s.cs.PairSnapshotByStations("alpha", "delta"); po != nil {
		t.Errorf("alpha|delta should be deleted by step; got %+v", po)
	}
	if st.ClockStepsResetPairs != 2 {
		t.Errorf("ClockStepsResetPairs=%d, want 2", st.ClockStepsResetPairs)
	}
}

// 3. A tiny backward movement INSIDE tolerance is accepted and does NOT
//    trip the guard. Software-clock jitter must not nuke pair state.
func TestMonotonicity_TinyBackwardAcceptedWithinTolerance(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{"alpha": 0, "bravo": 50_000.0, "delta": -30_000.0}
	// Warm-up.
	for i := 0; i < 5; i++ {
		c := s.mkAnchorCluster(uint32(1000+i),
			uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	// 500 us backward on alpha. Tolerance floor is 1 ms = 1_000_000 ns,
	// so this should be absorbed: no step detected, no pairs reset.
	tinyBack := s.mkAnchorCluster(2000,
		uint64(1_700_000_000_000_000_000+6*1_000_000),
		map[string]float64{"alpha": -500_000.0, "bravo": 50_000.0, "delta": -30_000.0})
	s.cs.FeedCluster(tinyBack)
	st := s.cs.Snapshot()
	if st.ClockStepsDetected != 0 {
		t.Errorf("tiny backward tripped guard: detected=%d", st.ClockStepsDetected)
	}
	if po := s.cs.PairSnapshotByStations("alpha", "bravo"); po == nil {
		t.Errorf("alpha|bravo should still exist under tiny backward jitter")
	}
}

// 4. A step on station alpha must NOT reset the bravo|delta pair, which
//    does not touch alpha. Guard scope is per-station, not global.
func TestMonotonicity_StepOnAlphaPreservesBravoDeltaPair(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{"alpha": 0, "bravo": 50_000.0, "delta": -30_000.0}
	for i := 0; i < 8; i++ {
		c := s.mkAnchorCluster(uint32(1000+i),
			uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	bdBefore := s.cs.PairSnapshotByStations("bravo", "delta")
	if bdBefore == nil {
		t.Fatal("warm-up: bravo|delta should exist")
	}
	stepCluster := s.mkAnchorCluster(2000,
		uint64(1_700_000_000_000_000_000+9*1_000_000),
		map[string]float64{"alpha": -1_000_000_000.0, "bravo": 50_000.0, "delta": -30_000.0})
	s.cs.FeedCluster(stepCluster)

	bdAfter := s.cs.PairSnapshotByStations("bravo", "delta")
	if bdAfter == nil {
		t.Fatalf("bravo|delta deleted by unrelated alpha step; guard scope is wrong")
	}
	// After alpha's obs was dropped, the surviving cluster has only 2 obs
	// (bravo + delta). FeedCluster's `len(usable) < 2` gate still passes
	// (>= 2 needed), so bravo|delta gets ONE additional sample. The pair
	// must still be converged from the warm-up.
	if bdAfter.Status != "converged" {
		t.Errorf("bravo|delta status=%q after alpha step, want still converged", bdAfter.Status)
	}
}

// 5. The clock-step event must surface through the existing
//    AnchorWarnings list with code "clock_step_detected" so the
//    /api/clock-sync/warnings endpoint and dashboard health strip pick
//    it up automatically.
func TestMonotonicity_WarningSurfacedViaAnchorWarnings(t *testing.T) {
	s := newScene(t, 0)
	offsets := map[string]float64{"alpha": 0, "bravo": 50_000.0, "delta": -30_000.0}
	for i := 0; i < 5; i++ {
		c := s.mkAnchorCluster(uint32(1000+i),
			uint64(1_700_000_000_000_000_000+i*1_000_000), offsets)
		s.cs.FeedCluster(c)
	}
	if len(s.cs.AnchorWarnings()) != 0 {
		t.Errorf("warm-up should not have produced any warnings; got %d",
			len(s.cs.AnchorWarnings()))
	}
	stepCluster := s.mkAnchorCluster(2000,
		uint64(1_700_000_000_000_000_000+6*1_000_000),
		map[string]float64{"alpha": -1_000_000_000.0, "bravo": 50_000.0, "delta": -30_000.0})
	s.cs.FeedCluster(stepCluster)

	warns := s.cs.AnchorWarnings()
	if len(warns) != 1 {
		t.Fatalf("AnchorWarnings count=%d after step, want 1: %+v", len(warns), warns)
	}
	w := warns[0]
	if w.Code != "clock_step_detected" {
		t.Errorf("warning code=%q, want clock_step_detected", w.Code)
	}
	if w.StationName != "alpha" {
		t.Errorf("warning station=%q, want alpha", w.StationName)
	}
	if !containsAll(w.Message, "alpha", "backward") {
		t.Errorf("warning message missing context: %q", w.Message)
	}
}
