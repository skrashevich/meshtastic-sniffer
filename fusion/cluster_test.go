// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (c) 2026 CEMAXECUTER LLC

package main

import (
	"testing"
	"time"
)

// Same-emission rule: a relay/retransmit of the same (from, packet_id)
// arriving outside the same-emission window must NOT be appended to the
// original cluster's observations. It must spawn a new emission cluster
// with a bumped EmissionSeq. Otherwise the solver gets two arrivals
// separated by seconds of relay delay and produces nonsense.
func TestSameEmission_RelayDoesNotMerge(t *testing.T) {
	pending := map[string][]*Cluster{}
	nextSeq := map[string]int{}
	const sameEmissionNs = uint64(200 * time.Millisecond / time.Nanosecond)

	base := uint64(1_000_000_000_000) // arbitrary epoch ns

	// Original RF emission heard by alpha at t=base.
	add := func(station string, lockTNs uint64, snr float64) *Cluster {
		f := Frame{From: "!aa", PacketID: 42, Station: station, PreambleLockTNs: lockTNs, SnrDB: snr,
			StationLat: 1, StationLon: 1, StationTNs: lockTNs}
		bk := clusterKey(f.From, f.PacketID)
		c := pickCluster(pending[bk], f, sameEmissionNs)
		now := time.Now()
		if c == nil {
			seq := nextSeq[bk]
			nextSeq[bk] = seq + 1
			c = &Cluster{Key: clusterKeyWithSeq(f.From, f.PacketID, seq), BaseKey: bk, EmissionSeq: seq, FirstSeen: now, Frame: f}
			if f.PreambleLockTNs == 0 {
				c.LowTrust = true
			}
			pending[bk] = append(pending[bk], c)
		} else if f.PreambleLockTNs == 0 {
			c.LowTrust = true
		}
		c.mergeObservation(Observation{Station: station, StationLat: 1, StationLon: 1, StationTNs: lockTNs,
			PreambleLockTNs: lockTNs, SnrDB: snr, At: now})
		return c
	}

	// alpha heard original at t=base
	c0 := add("alpha", base, 10)
	// bravo heard original 5us later (well within 200ms window)
	c1 := add("bravo", base+5_000, 8)
	if c0 != c1 {
		t.Fatalf("alpha and bravo should share emission seq=0; got distinct clusters")
	}
	if got := c1.EmissionSeq; got != 0 {
		t.Errorf("expected EmissionSeq=0 for original emission, got %d", got)
	}
	if len(pending[clusterKey("!aa", 42)]) != 1 {
		t.Fatalf("pending should hold 1 cluster after original, got %d",
			len(pending[clusterKey("!aa", 42)]))
	}

	// charlie heard a relay 3 seconds later (way outside 200ms)
	c2 := add("charlie", base+3_000_000_000, 9)
	if c2 == c0 {
		t.Fatalf("relay 3s later must NOT join original cluster; same-emission gate failed")
	}
	if c2.EmissionSeq != 1 {
		t.Errorf("expected relay EmissionSeq=1, got %d", c2.EmissionSeq)
	}
	if got := len(pending[clusterKey("!aa", 42)]); got != 2 {
		t.Fatalf("pending should hold 2 clusters after relay, got %d", got)
	}
	if len(c0.Observations) != 2 {
		t.Errorf("original cluster should have 2 obs (alpha+bravo), got %d", len(c0.Observations))
	}
	if len(c2.Observations) != 1 {
		t.Errorf("relay cluster should have 1 obs (charlie), got %d", len(c2.Observations))
	}
}

// Per-station dedup: when the same station produces two observations of
// one RF emission (e.g. wideband + focused-pool decode), only one should
// land in the cluster. Ranking: higher class (lock-bearing > frame-only)
// first, then higher SNR.
func TestPerStationDedup_KeepsBestObservation(t *testing.T) {
	c := &Cluster{Key: "!aa|1", BaseKey: "!aa|1", FirstSeen: time.Now(), Frame: Frame{From: "!aa", PacketID: 1}}

	// Wideband decode: lock present, lower SNR
	c.mergeObservation(Observation{Station: "alpha", PreambleLockTNs: 1_000_000_000_000, SnrDB: 6})
	if len(c.Observations) != 1 {
		t.Fatalf("first obs should append; got %d", len(c.Observations))
	}
	if c.StationDupesSuppressed != 0 {
		t.Errorf("no dupe yet; got suppressed=%d", c.StationDupesSuppressed)
	}

	// Focused decode same station, lock present, higher SNR -> wins
	c.mergeObservation(Observation{Station: "alpha", PreambleLockTNs: 1_000_000_000_001, SnrDB: 14})
	if len(c.Observations) != 1 {
		t.Fatalf("dupe should not grow obs count; got %d", len(c.Observations))
	}
	if c.StationDupesSuppressed != 1 {
		t.Errorf("dupe should bump counter to 1; got %d", c.StationDupesSuppressed)
	}
	if c.Observations[0].SnrDB != 14 {
		t.Errorf("higher-SNR obs should win; got SnrDB=%v", c.Observations[0].SnrDB)
	}

	// Different station, lock-free, lower SNR -> appends fresh.
	c.mergeObservation(Observation{Station: "bravo", PreambleLockTNs: 0, SnrDB: 3})
	if len(c.Observations) != 2 {
		t.Fatalf("distinct station should append; got %d", len(c.Observations))
	}

	// Class beats SNR: frame-only higher-SNR collides with lock-bearing
	// lower-SNR. Keep the lock-bearing one.
	c2 := &Cluster{Key: "!bb|1", BaseKey: "!bb|1", FirstSeen: time.Now(), Frame: Frame{From: "!bb", PacketID: 1}}
	c2.mergeObservation(Observation{Station: "alpha", PreambleLockTNs: 999, SnrDB: 4}) // lock, low SNR
	c2.mergeObservation(Observation{Station: "alpha", PreambleLockTNs: 0, SnrDB: 20})  // frame-only, high SNR
	if c2.Observations[0].PreambleLockTNs != 999 {
		t.Errorf("lock-bearing obs should beat frame-only despite lower SNR; got lock=%d", c2.Observations[0].PreambleLockTNs)
	}
	if c2.StationDupesSuppressed != 1 {
		t.Errorf("expected suppressed=1; got %d", c2.StationDupesSuppressed)
	}
}

// Missing-lock fallback: when preamble_lock_t_ns is absent, the cluster
// must still accept the observation (legacy wall-clock matching) but be
// flagged LowTrust so downstream solve-quality reporting can degrade.
func TestMissingLock_MarksLowTrust(t *testing.T) {
	pending := map[string][]*Cluster{}
	nextSeq := map[string]int{}
	const sameEmissionNs = uint64(200 * time.Millisecond / time.Nanosecond)

	add := func(station string, lockTNs uint64) *Cluster {
		f := Frame{From: "!aa", PacketID: 7, Station: station, PreambleLockTNs: lockTNs, StationTNs: 1, StationLat: 1, StationLon: 1}
		bk := clusterKey(f.From, f.PacketID)
		c := pickCluster(pending[bk], f, sameEmissionNs)
		now := time.Now()
		if c == nil {
			seq := nextSeq[bk]
			nextSeq[bk] = seq + 1
			c = &Cluster{Key: clusterKeyWithSeq(f.From, f.PacketID, seq), BaseKey: bk, EmissionSeq: seq, FirstSeen: now, Frame: f}
			if f.PreambleLockTNs == 0 {
				c.LowTrust = true
			}
			pending[bk] = append(pending[bk], c)
		} else if f.PreambleLockTNs == 0 {
			c.LowTrust = true
		}
		c.mergeObservation(Observation{Station: station, StationLat: 1, StationLon: 1, StationTNs: 1, PreambleLockTNs: lockTNs, At: now})
		return c
	}

	// First obs has no lock -> cluster is born LowTrust.
	c := add("alpha", 0)
	if !c.LowTrust {
		t.Errorf("cluster spawned with lock-free first obs must be LowTrust")
	}
	if c.MinPreambleLockTNs != 0 || c.MaxPreambleLockTNs != 0 {
		t.Errorf("lock bounds should be zero; got min=%d max=%d", c.MinPreambleLockTNs, c.MaxPreambleLockTNs)
	}

	// Second obs WITH lock joins the same cluster (Min=Max=0 path).
	c2 := add("bravo", 5_000_000_000_000)
	if c2 != c {
		t.Fatalf("lock-bearing bravo should join existing low-trust cluster; got new cluster")
	}
	if !c.LowTrust {
		t.Errorf("LowTrust must persist after lock-bearing join; got false")
	}
	if c.MinPreambleLockTNs == 0 || c.MaxPreambleLockTNs == 0 {
		t.Errorf("lock bounds should track bravo's lock; got min=%d max=%d", c.MinPreambleLockTNs, c.MaxPreambleLockTNs)
	}

	// Third obs lock-free again to a separate baseKey -> separate cluster, LowTrust.
	pending2 := map[string][]*Cluster{}
	nextSeq2 := map[string]int{}
	f := Frame{From: "!bb", PacketID: 9, Station: "delta"}
	bk := clusterKey(f.From, f.PacketID)
	d := pickCluster(pending2[bk], f, sameEmissionNs)
	if d != nil {
		t.Fatalf("empty pending must return nil for lock-free pickCluster")
	}
	seq := nextSeq2[bk]
	nextSeq2[bk] = seq + 1
	d = &Cluster{BaseKey: bk, EmissionSeq: seq, FirstSeen: time.Now(), Frame: f, LowTrust: true}
	pending2[bk] = append(pending2[bk], d)
	d.mergeObservation(Observation{Station: "delta"})
	if !d.LowTrust {
		t.Errorf("fresh lock-free cluster must be LowTrust; got false")
	}
}

// flushReady must walk the slice-of-clusters per baseKey, releasing only
// the ones whose wall-clock age exceeds the window, and deleting the
// baseKey entry only when every cluster underneath it has flushed.
func TestFlushReady_MultipleClustersPerBaseKey(t *testing.T) {
	pending := map[string][]*Cluster{}
	now := time.Now()
	bk := "!aa|1"

	older := &Cluster{BaseKey: bk, EmissionSeq: 0, FirstSeen: now.Add(-10 * time.Second)}
	newer := &Cluster{BaseKey: bk, EmissionSeq: 1, FirstSeen: now.Add(-1 * time.Second)}
	pending[bk] = []*Cluster{older, newer}

	ready := flushReady(pending, 5*time.Second, now)
	if len(ready) != 1 {
		t.Fatalf("expected 1 cluster ready, got %d", len(ready))
	}
	if ready[0] != older {
		t.Errorf("expected older cluster to flush; got EmissionSeq=%d", ready[0].EmissionSeq)
	}
	if got := len(pending[bk]); got != 1 {
		t.Fatalf("expected newer cluster to stay pending; got len=%d", got)
	}

	ready2 := flushReady(pending, 5*time.Second, now.Add(5*time.Second))
	if len(ready2) != 1 {
		t.Fatalf("expected newer cluster to flush after wall-clock advances; got %d", len(ready2))
	}
	if _, present := pending[bk]; present {
		t.Errorf("baseKey entry should be deleted when all clusters flushed")
	}
}
