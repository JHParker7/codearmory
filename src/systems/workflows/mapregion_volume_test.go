package main

import "testing"

// Two map regions that clone the SAME base volume must not collide on their iteration
// clones. Declaring `volume: workspace` in both regions is the normal thing to do —
// they are both cloning the checkout — and the names used to be keyed only on the
// iteration index, so per-service leg 0 and per-extra leg 0 were both "workspace-m0".
//
// Sharing the name is not a benign aliasing bug: an iteration DELETES its clone when
// it finishes, so whichever region finished first destroyed the volume the other
// region's still-running leg was mounted on. In codearmory-ci that appeared as the
// image build failing with `volume "workspace-m0" not found ... (create it first)`
// while the very same leg's test step had just passed on it.
//
// It also needs BOTH regions to be non-empty before it can happen, so a pipeline runs
// green until the first push that changes a service AND a non-service module.
func TestIterVolumeDistinctPerRegion(t *testing.T) {
	// firstIndex is the region key: the workflow-level index of the region's first
	// step. Two regions can never share one, since a step belongs to at most one.
	const perServiceFirst, perExtraFirst = 7, 9

	a := iterVolume("workspace", perServiceFirst, 0)
	b := iterVolume("workspace", perExtraFirst, 0)
	if a == b {
		t.Fatalf("both regions' leg 0 resolved to %q — one region's cleanup deletes the other's live volume", a)
	}

	// Within a region, legs still have to differ, or parallel iterations would share
	// one ReadWriteOnce clone.
	if x, y := iterVolume("workspace", perServiceFirst, 0), iterVolume("workspace", perServiceFirst, 1); x == y {
		t.Errorf("legs 0 and 1 of one region both resolved to %q", x)
	}

	// Deterministic: the body mounts the volume by name, so the same inputs must give
	// the same name every time it is derived.
	if x, y := iterVolume("workspace", perServiceFirst, 2), iterVolume("workspace", perServiceFirst, 2); x != y {
		t.Errorf("not deterministic: %q vs %q", x, y)
	}

	// forge validates a volume name as a DNS-1123 label capped at 40 chars, so the
	// derived name has to stay inside that. This is why the region's author-chosen id
	// is not interpolated here — a long map id would push the name over the cap and
	// fail at create time rather than at author time.
	for _, name := range []string{
		iterVolume("workspace", perServiceFirst, 0),
		iterVolume("workspace", 999, 999),
	} {
		if len(name) > 40 {
			t.Errorf("volume name %q is %d chars; forge caps a volume name at 40", name, len(name))
		}
		for _, c := range name {
			isLower := c >= 'a' && c <= 'z'
			isDigit := c >= '0' && c <= '9'
			if !isLower && !isDigit && c != '-' {
				t.Errorf("volume name %q contains %q, which is not valid in a DNS-1123 label", name, c)
				break
			}
		}
	}
}
