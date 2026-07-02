// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (c) 2026 CEMAXECUTER LLC
//
// fusion/seed_evidence_dev.go: dev-only seeder for the Evidence-tab UI.
//
// Gated behind --seed-evidence-dev. Writes a curated set of synthetic
// rows into the cluster_observations, solved_fixes, pair_snapshots
// buckets plus a clock-sync warning so the dashboard UI can be designed
// against realistic shapes BEFORE live ZMQ feeds are connected.
//
// Not part of any production path. The flag log-warns loudly on use.
// The seven scenarios mirror the cases an operator should be able to
// distinguish in the timeline + detail pane:
//
//   1. unsolved cluster (2-station; insufficient for TDOA)
//   2. solved fix (sync class, low uncertainty, all the pretty fields)
//   3. degraded solve (software_lock class, higher uncertainty, mixed timing)
//   4. clock-sync warning (anchor placement)
//   5. duplicate-station observation suppressed (dupes counter > 0)
//   6. relay-separated event (same from+pid, two emission_seq values)
//   7. snapshot-keys-used present vs absent (solve detail variety)
//
// Timestamps are anchored ~5 minutes before "now" so a 15m zoom catches
// everything. Synthetic event_time_ns and cluster_time_ns line up so
// timeline rows pair with their fixes.

package main

import (
	"fmt"
	"log"
	"time"
)

func seedEvidenceFixtures(store *EventStore, cs *ClockSync) error {
	if store == nil {
		return fmt.Errorf("nil store")
	}
	now := time.Now()
	base := uint64(now.Add(-5*time.Minute).UnixNano())

	station := func(name string, lat, lon float64, lockNs uint64, snr float64) ClusterObservationStation {
		return ClusterObservationStation{
			Station: name, StationLat: lat, StationLon: lon,
			StationTNs: lockNs + 5_000_000, // frame timestamp ~5 ms after lock
			StationTAccNs:   1_000_000,
			PreambleLockTNs: lockNs,
			SnrDB:           snr,
			RssiDB:          -94.0,
		}
	}

	// 1. UNSOLVED CLUSTER -- only 2 stations heard it; insufficient for TDOA.
	rec1 := &ClusterObservationRecord{
		From: "!a0a0a0a0", PacketID: 101, EmissionSeq: 0,
		ClusterTimeNs: base, FirstSeenWallNs: base + 100_000,
		Preset: "LongFast", SF: 11, CR: 5, BwHz: 125_000, FreqHz: 906_875_000,
		ChannelName: "LongFast",
		Observations: []ClusterObservationStation{
			station("alpha", 39.010, -98.010, base+0, 4.2),
			station("bravo", 39.000, -98.012, base+95_000, -1.5),
		},
	}
	if err := store.WriteClusterObservation(rec1); err != nil {
		return fmt.Errorf("seed unsolved cluster: %w", err)
	}

	// 2. SOLVED FIX (sync class, low uncertainty, full clock-sync diagnostics).
	t2 := base + uint64(45*time.Second)
	rec2 := &ClusterObservationRecord{
		From: "!b1b1b1b1", PacketID: 202, EmissionSeq: 0,
		ClusterTimeNs: t2, FirstSeenWallNs: t2 + 100_000,
		Preset: "MediumFast", SF: 9, CR: 5, BwHz: 250_000, FreqHz: 906_875_000,
		ChannelName: "primary",
		Observations: []ClusterObservationStation{
			station("alpha", 39.010, -98.010, t2+0, 8.4),
			station("bravo", 39.000, -98.012, t2+18_500, 6.1),
			station("delta", 38.988, -98.002, t2+24_300, 7.8),
		},
	}
	if err := store.WriteClusterObservation(rec2); err != nil {
		return fmt.Errorf("seed solved cluster: %w", err)
	}
	fix2 := &SolvedFixRecord{
		EventTimeNs: t2, SolutionTimeNs: t2 + 12_000_000,
		From: "!b1b1b1b1", PacketID: 202, EmissionSeq: 0,
		ClusterKey:           "!b1b1b1b1|202",
		Lat:                  38.999, Lon: -98.005,
		UncertaintyM:         18.4,
		StationCount:         3,
		Iterations:           6,
		TimestampClass:       "sync",
		Degraded:             false,
		ClockSyncPairCount:   2,
		ClockSyncResidualNs:  340.0,
		ClockSyncAnchorCount: 1,
		ClockSyncReference:   "alpha",
		PairKeysConsidered:   []string{"alpha|bravo", "alpha|delta"},
		PairSnapshotKeysUsed: []string{"alpha|bravo", "alpha|delta"},
		RawGeolocatedJSON:    []byte(`{"event":"GEOLOCATED","from":"!b1b1b1b1"}`),
	}
	if err := store.WriteSolvedFix(fix2); err != nil {
		return fmt.Errorf("seed solved fix: %w", err)
	}

	// 3. DEGRADED SOLVE (software_lock class; mixed timing; higher uncertainty;
	//    no pair_snapshot_keys_used to exercise the "snapshots missing" detail).
	t3 := base + uint64(90*time.Second)
	rec3 := &ClusterObservationRecord{
		From: "!c2c2c2c2", PacketID: 303, EmissionSeq: 0,
		ClusterTimeNs: t3, FirstSeenWallNs: t3 + 100_000,
		Preset: "ShortFast", SF: 7, CR: 5, BwHz: 250_000, FreqHz: 906_875_000,
		ChannelName: "primary",
		Observations: []ClusterObservationStation{
			station("alpha", 39.010, -98.010, t3+0, 3.0),
			station("bravo", 39.000, -98.012, t3+47_200, 1.7),
			station("delta", 38.988, -98.002, t3+62_500, 0.4),
		},
	}
	if err := store.WriteClusterObservation(rec3); err != nil {
		return fmt.Errorf("seed degraded cluster: %w", err)
	}
	fix3 := &SolvedFixRecord{
		EventTimeNs: t3, SolutionTimeNs: t3 + 22_000_000,
		From: "!c2c2c2c2", PacketID: 303, EmissionSeq: 0,
		ClusterKey:     "!c2c2c2c2|303",
		Lat:            38.994, Lon: -98.000,
		UncertaintyM:   86.3,
		StationCount:   3,
		Iterations:     11,
		TimestampClass: "software_lock",
		Degraded:       true,
		// No clock-sync converged here -> zero diagnostics, no pair snapshot keys.
	}
	if err := store.WriteSolvedFix(fix3); err != nil {
		return fmt.Errorf("seed degraded fix: %w", err)
	}

	// 4. DUPLICATE-STATION OBSERVATION SUPPRESSED. Same (from, pid) as a
	//    different RF event; one station fired wideband + focused-pool and the
	//    cluster builder kept the better one + bumped the counter.
	t5 := base + uint64(150*time.Second)
	rec5 := &ClusterObservationRecord{
		From: "!d3d3d3d3", PacketID: 505, EmissionSeq: 0,
		ClusterTimeNs: t5, FirstSeenWallNs: t5 + 100_000,
		Preset: "LongFast", SF: 11, CR: 5, BwHz: 125_000, FreqHz: 906_875_000,
		ChannelName:            "LongFast",
		StationDupesSuppressed: 2,
		Observations: []ClusterObservationStation{
			station("alpha", 39.010, -98.010, t5+0, 9.1),
			station("bravo", 39.000, -98.012, t5+15_400, 5.6),
			station("delta", 38.988, -98.002, t5+22_100, 7.0),
		},
	}
	if err := store.WriteClusterObservation(rec5); err != nil {
		return fmt.Errorf("seed dupes cluster: %w", err)
	}

	// 5+6. RELAY-SEPARATED EVENT. Same (from, packet_id), two distinct
	//      emissions: original at t6a, mesh relay 4 seconds later at t6b. The
	//      same-emission gate spawned EmissionSeq=1; both rows persist.
	t6a := base + uint64(200*time.Second)
	t6b := t6a + uint64(4*time.Second)
	relayFrom, relayPid := "!e4e4e4e4", uint32(606)
	recRelayOrig := &ClusterObservationRecord{
		From: relayFrom, PacketID: relayPid, EmissionSeq: 0,
		ClusterTimeNs: t6a, FirstSeenWallNs: t6a + 100_000,
		Preset: "MediumFast", SF: 9, CR: 5, BwHz: 250_000, FreqHz: 906_875_000,
		ChannelName: "primary",
		Observations: []ClusterObservationStation{
			station("alpha", 39.010, -98.010, t6a+0, 7.5),
			station("bravo", 39.000, -98.012, t6a+19_800, 6.0),
		},
	}
	recRelayCopy := &ClusterObservationRecord{
		From: relayFrom, PacketID: relayPid, EmissionSeq: 1,
		ClusterTimeNs: t6b, FirstSeenWallNs: t6b + 100_000,
		Preset: "MediumFast", SF: 9, CR: 5, BwHz: 250_000, FreqHz: 906_875_000,
		ChannelName: "primary",
		Observations: []ClusterObservationStation{
			station("delta", 38.988, -98.002, t6b+0, 4.8),
			station("bravo", 39.000, -98.012, t6b+12_700, 3.2),
		},
	}
	if err := store.WriteClusterObservation(recRelayOrig); err != nil {
		return fmt.Errorf("seed relay original: %w", err)
	}
	if err := store.WriteClusterObservation(recRelayCopy); err != nil {
		return fmt.Errorf("seed relay copy: %w", err)
	}

	// 7. CLOCK-SYNC WARNING. Push an anchor-too-close warning so the banner
	//    renders next to the timeline. This works even when clock-sync was
	//    auto-disabled (no anchors declared) because we set warnings directly
	//    on the ClockSync object. When clock-sync IS off the warning still
	//    surfaces via /api/clock-sync/warnings as long as the handler can see
	//    globalClockSync; if globalClockSync is nil we silently skip the
	//    warning -- the timeline will still show the other six fixtures.
	if cs != nil {
		existing := cs.AnchorWarnings()
		seeded := ClockSyncWarning{
			Code:        "anchor_too_close",
			AnchorID:    "!cafe1234",
			StationName: "alpha",
			DistanceM:   12.4,
			MinM:        30.0,
			Message: "anchor !cafe1234 is 12.4 m from sniffer station \"alpha\" " +
				"(<30 m). Clock-sync samples from this pair will likely be biased by " +
				"near-field / front-end saturation. (dev-seeded)",
		}
		cs.SetAnchorWarnings(append(existing, seeded))
	}

	// pair_snapshots: write the two pair states the sync solve referenced so
	//                 the Evidence detail "Pair Snapshots Used" section reads
	//                 a real bbolt row when the user clicks the solved fix.
	for _, ps := range []*PairSnapshotRecord{
		{
			PairKey:           "alpha|bravo",
			SnapshotTimeNs:    t2,
			LastAnchorTimeNs:  t2 - uint64(30*time.Second),
			MedianNs:          18500.0,
			MadNs:             310.0,
			SampleCount:       12,
			AnchorIDs:         []string{"!cafe1234"},
			StatusAtSnapshot:  "converged",
			MaxAgeS:           600.0,
		},
		{
			PairKey:           "alpha|delta",
			SnapshotTimeNs:    t2,
			LastAnchorTimeNs:  t2 - uint64(28*time.Second),
			MedianNs:          24300.0,
			MadNs:             280.0,
			SampleCount:       11,
			AnchorIDs:         []string{"!cafe1234"},
			StatusAtSnapshot:  "converged",
			MaxAgeS:           600.0,
		},
	} {
		if err := store.WritePairSnapshot(ps); err != nil {
			return fmt.Errorf("seed pair snapshot: %w", err)
		}
	}

	log.Printf("seed-evidence-dev: wrote 7 fixture rows (2 fixes, 5 clusters, 2 pair snapshots, 1 warning)")
	return nil
}
