package main

import "testing"

func legVolumes(t *testing.T, c *ScatterConfig, with map[string]any) []map[string]any {
	t.Helper()
	raw := scatterLegVolumes(with, c, "run-1", "workspace-s0")
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("volume entry is not a map: %#v", v)
		}
		out = append(out, m)
	}
	return out
}

// The motivating case: a leg must be able to carry a SHARED cache volume alongside its
// workspace. Before this, scatter replaced `volumes` outright and the cache was silently
// dropped, so every leg started from a cold build cache.
func TestScatterLegVolumes_CarriesExtraVolumesThrough(t *testing.T) {
	c := &ScatterConfig{}
	with := map[string]any{"volumes": []any{
		map[string]any{"workflow_id": "run-1", "name": "gocache", "mount_path": "/gocache"},
	}}

	vols := legVolumes(t, c, with)
	if len(vols) != 2 {
		t.Fatalf("want workspace + cache, got %d volumes: %#v", len(vols), vols)
	}
	if vols[0]["name"] != "workspace-s0" || vols[0]["workdir"] != true {
		t.Errorf("first volume should be the leg's clone as workdir, got %#v", vols[0])
	}
	if vols[1]["name"] != "gocache" || vols[1]["mount_path"] != "/gocache" {
		t.Errorf("cache volume should be carried through, got %#v", vols[1])
	}
}

// Two mounts at one path is invalid, and the clone replaces the base — so a declared
// volume that collides with either must be dropped rather than produce a broken pod.
func TestScatterLegVolumes_DropsCollidingVolumes(t *testing.T) {
	c := &ScatterConfig{Volume: "workspace", MountPath: "/workspace"}
	with := map[string]any{"volumes": []any{
		map[string]any{"name": "workspace", "mount_path": "/elsewhere"}, // the base itself
		map[string]any{"name": "other", "mount_path": "/workspace"},     // the workspace mount
		map[string]any{"name": "nomount"},                               // no mount_path
	}}

	vols := legVolumes(t, c, with)
	if len(vols) != 1 {
		t.Fatalf("all three should be dropped, leaving only the clone; got %#v", vols)
	}
}

// An extra volume must not steal workdir from the leg's workspace.
func TestScatterLegVolumes_StripsWorkdirFromExtras(t *testing.T) {
	c := &ScatterConfig{}
	with := map[string]any{"volumes": []any{
		map[string]any{"name": "gocache", "mount_path": "/gocache", "workdir": true},
	}}

	vols := legVolumes(t, c, with)
	if len(vols) != 2 {
		t.Fatalf("want 2 volumes, got %#v", vols)
	}
	if _, ok := vols[1]["workdir"]; ok {
		t.Errorf("workdir should be stripped from the extra volume, got %#v", vols[1])
	}
	// The step definition is shared across legs — it must not be mutated in place.
	orig := with["volumes"].([]any)[0].(map[string]any)
	if orig["workdir"] != true {
		t.Errorf("the caller's step definition was mutated: %#v", orig)
	}
}

// share_base: the leg mounts the BASE read-only, and there is no clone volume at all.
func TestScatterLegVolumes_ShareBaseMountsBaseReadOnly(t *testing.T) {
	c := &ScatterConfig{ShareBase: true}
	vols := legVolumes(t, c, map[string]any{})

	if len(vols) != 1 {
		t.Fatalf("want just the shared base, got %#v", vols)
	}
	v := vols[0]
	if v["name"] != defaultScatterVolume {
		t.Errorf("share_base leg should mount the base volume, got %#v", v)
	}
	if v["read_only"] != true {
		t.Errorf("share_base leg must mount the base READ-ONLY, got %#v", v)
	}
}

// share_base has no per-leg volume, so there is nothing for a gather to read: reject the
// combination up front instead of silently dropping the caller's outputs.
func TestValidateScatter_ShareBaseRejectsOutputs(t *testing.T) {
	msg := validateScatter(&ScatterConfig{Regex: "x", ShareBase: true, Outputs: []string{"bin"}})
	if msg == "" {
		t.Fatal("share_base + outputs should be rejected")
	}
	// And it is fine without outputs.
	if msg := validateScatter(&ScatterConfig{Regex: "x", ShareBase: true}); msg != "" {
		t.Fatalf("share_base without outputs should be valid, got %q", msg)
	}
}
