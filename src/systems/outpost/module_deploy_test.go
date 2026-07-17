package main

import (
	"context"
	"testing"
)

func TestDeployModule_Name(t *testing.T) {
	if newDeployModule(nil).Name() != "deploy" {
		t.Fatal("name must be deploy — it keys command routing")
	}
}

// Validation runs before any cluster call, so a nil client is fine here: a malformed
// command must be rejected, not sent to the API server.
func TestDeployModule_CommandValidation(t *testing.T) {
	m := newDeployModule(nil)
	ctx := context.Background()
	cases := []struct {
		name string
		cmd  Command
		want string
	}{
		{"unknown type", Command{Type: "nope"}, "unknown command type"},
		{"rollout no deployment", Command{Type: "rollout", Payload: map[string]any{}}, "missing deployment"},
		{"set-image no image", Command{Type: "set-image", Payload: map[string]any{"deployment": "x"}}, "requires deployment and image"},
		{"set-image no deployment", Command{Type: "set-image", Payload: map[string]any{"image": "y"}}, "requires deployment and image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.HandleCommand(ctx, tc.cmd)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A command's namespace overrides the module default; absent, the default applies —
// so a redeploy lands in the right namespace whether or not the caller specifies one.
func TestDeployModule_NamespaceResolution(t *testing.T) {
	m := &deployModule{defaultNS: "codearmory"}
	if got := m.namespace(Command{Payload: map[string]any{"namespace": "staging"}}); got != "staging" {
		t.Errorf("explicit namespace = %q, want staging", got)
	}
	if got := m.namespace(Command{Payload: map[string]any{}}); got != "codearmory" {
		t.Errorf("default namespace = %q, want codearmory", got)
	}
}

// A strategic merge on containers is keyed by name, so set-image needs exactly one
// container to default the name safely — this is the extraction that decides that.
func TestUnstructuredContainers(t *testing.T) {
	dep := func(names ...string) map[string]any {
		var cs []any
		for _, n := range names {
			cs = append(cs, map[string]any{"name": n, "image": "old"})
		}
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": cs}}}}
	}
	c, ok, _ := unstructuredContainers(dep("only"))
	if !ok || len(c) != 1 {
		t.Errorf("single container: ok=%v len=%d", ok, len(c))
	}
	c, ok, _ = unstructuredContainers(dep("a", "b"))
	if !ok || len(c) != 2 {
		t.Errorf("two containers: ok=%v len=%d", ok, len(c))
	}
	// A malformed deployment must not panic — it returns not-found, and set-image then
	// errors asking for an explicit container rather than guessing.
	if _, ok, _ := unstructuredContainers(map[string]any{}); ok {
		t.Error("empty object must report no containers")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
