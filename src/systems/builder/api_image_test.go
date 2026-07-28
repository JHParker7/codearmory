package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func strp(s string) *string { return &s }

// retag must preserve the registry and repository. The awkward case is a registry
// with a port — "host:5000/repo" has a colon that is NOT a tag separator, and
// treating it as one would rewrite the port and point the pull at nothing.
func TestRetag(t *testing.T) {
	cases := []struct{ in, tag, want string }{
		{"registry/app:dev", "abc123", "registry/app:abc123"},
		{"registry/app", "abc123", "registry/app:abc123"},
		{"192.168.53.171:3000/jp01/git-factory:dev", "abc123", "192.168.53.171:3000/jp01/git-factory:abc123"},
		// A port and no tag: the colon belongs to the host, so a tag is appended.
		{"192.168.53.171:3000/jp01/git-factory", "abc123", "192.168.53.171:3000/jp01/git-factory:abc123"},
		// A digest pin is deliberately left alone — retagging a digest is meaningless.
		{"registry/app@sha256:deadbeef", "abc123", "registry/app@sha256:deadbeef"},
		{"registry/app:dev", "", "registry/app:dev"},
		{"", "abc123", ""},
	}
	for _, c := range cases {
		if got := retag(c.in, c.tag); got != c.want {
			t.Errorf("retag(%q,%q) = %q, want %q", c.in, c.tag, got, c.want)
		}
	}
}

func podWith(image string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "svc", Image: image}}},
	}
}

// The critical property for the cloned Helm base: with no override requested the
// chart's image must be left exactly as rendered. Rewriting it unconditionally
// would swap the chart's registry for builder's default on every platform service.
func TestApplyImageOverrides_NoOverrideLeavesChartImageAlone(t *testing.T) {
	pt := podWith("ghcr.io/code-armory-app/git-factory:1.2.3")
	applyImageOverrides(pt, workloadSpec{Service: "codearmory_git_factory"})
	if got := pt.Spec.Containers[0].Image; got != "ghcr.io/code-armory-app/git-factory:1.2.3" {
		t.Errorf("image = %q, want the chart's image untouched", got)
	}
	if pt.Spec.Containers[0].ImagePullPolicy != "" {
		t.Errorf("pull policy = %q, want unset", pt.Spec.Containers[0].ImagePullPolicy)
	}
}

// A tag override retags the image the service would otherwise run, keeping its
// registry and repository — which is what makes it safe on a cloned chart base.
func TestApplyImageOverrides_TagRetagsPreservingRegistry(t *testing.T) {
	pt := podWith("192.168.53.171:3000/jp01/git-factory:dev")
	applyImageOverrides(pt, workloadSpec{Tag: "a0fd9b9"})
	if got := pt.Spec.Containers[0].Image; got != "192.168.53.171:3000/jp01/git-factory:a0fd9b9" {
		t.Errorf("image = %q, want the tag swapped and the registry kept", got)
	}
}

func TestApplyImageOverrides_ExplicitImageWinsOverTag(t *testing.T) {
	pt := podWith("registry/app:dev")
	applyImageOverrides(pt, workloadSpec{Image: "local/git-factory:test", Tag: "ignored"})
	if got := pt.Spec.Containers[0].Image; got != "local/git-factory:test" {
		t.Errorf("image = %q, want the explicit image", got)
	}
}

// The local-image case: an image built onto the node must never be pulled.
func TestApplyImageOverrides_PullPolicyForLocalImage(t *testing.T) {
	pt := podWith("registry/app:dev")
	applyImageOverrides(pt, workloadSpec{Image: "git-factory:local", PullPolicy: "Never"})
	c := pt.Spec.Containers[0]
	if c.Image != "git-factory:local" || c.ImagePullPolicy != corev1.PullNever {
		t.Errorf("got %q/%q, want git-factory:local/Never", c.Image, c.ImagePullPolicy)
	}
}

func TestApplyImageOverrides_NoContainersIsSafe(t *testing.T) {
	pt := &corev1.PodTemplateSpec{}
	applyImageOverrides(pt, workloadSpec{Tag: "x"}) // must not panic
}

func TestValidateImageRequest(t *testing.T) {
	cases := []struct {
		name    string
		req     setImageRequest
		wantErr bool
	}{
		{"a commit sha tag", setImageRequest{Tag: strp("a0fd9b9")}, false},
		{"dots and dashes", setImageRequest{Tag: strp("v1.2.3-rc1")}, false},
		{"clearing the tag", setImageRequest{Tag: strp("")}, false},
		{"valid pull policy", setImageRequest{PullPolicy: strp("Never")}, false},
		// A tag containing a slash or colon would silently retarget the pull, because
		// it is interpolated straight into an image reference.
		{"tag with a slash", setImageRequest{Tag: strp("evil/repo")}, true},
		{"tag with a colon", setImageRequest{Tag: strp("a:b")}, true},
		{"tag opening with a dot", setImageRequest{Tag: strp(".hidden")}, true},
		{"bogus pull policy", setImageRequest{PullPolicy: strp("Sometimes")}, true},
		{"lowercase pull policy", setImageRequest{PullPolicy: strp("never")}, true},
		{"image with whitespace", setImageRequest{Image: strp("repo:tag extra")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := validateImageRequest(c.req)
			if (msg != "") != c.wantErr {
				t.Errorf("validateImageRequest(%+v) = %q, wantErr=%v", c.req, msg, c.wantErr)
			}
		})
	}
}

// imageFor is the no-base path: explicit image, else registry/repo at the pinned
// tag, else registry/repo at the backend default.
func TestImageFor_Precedence(t *testing.T) {
	b := &k8sBackend{registry: "ghcr.io/acme", tag: "latest"}
	if got := b.imageFor(workloadSpec{Service: "svc", Image: "explicit:1"}); got != "explicit:1" {
		t.Errorf("explicit image: got %q", got)
	}
	if got := b.imageFor(workloadSpec{Service: "svc", Tag: "abc123"}); got != "ghcr.io/acme/svc:abc123" {
		t.Errorf("pinned tag: got %q, want ghcr.io/acme/svc:abc123", got)
	}
	if got := b.imageFor(workloadSpec{Service: "svc"}); got != "ghcr.io/acme/svc:latest" {
		t.Errorf("default tag: got %q, want ghcr.io/acme/svc:latest", got)
	}
}

// The gap that made the per-service knob incomplete: with only a global registry,
// Tag composes against BUILDER_IMAGE_REGISTRY. On an install whose global points
// somewhere this service does NOT publish — exactly this cluster, where the global
// is ghcr.io/code-armory-app but images live on the Forgejo registry — every tag
// resolved to an unpullable reference, and the only escape was restating the whole
// image on every deploy. Registry + Tag make the image fully per-service.
func TestImageFor_PerServiceRegistryMakesTagUsable(t *testing.T) {
	b := &k8sBackend{registry: "ghcr.io/code-armory-app", tag: "alpha-0.1.0"}

	// Without a per-service registry, a tag composes against the (wrong) global.
	if got := b.imageFor(workloadSpec{Service: "svc", Tag: "704caa9"}); got != "ghcr.io/code-armory-app/svc:704caa9" {
		t.Errorf("global registry: got %q", got)
	}
	// With one, the service is fully described on its own terms.
	got := b.imageFor(workloadSpec{Service: "svc", Registry: "192.168.53.171:3000/jp01", Tag: "704caa9"})
	if got != "192.168.53.171:3000/jp01/svc:704caa9" {
		t.Errorf("per-service registry: got %q, want 192.168.53.171:3000/jp01/svc:704caa9", got)
	}
	// Registry alone still honours the platform tag.
	if got := b.imageFor(workloadSpec{Service: "svc", Registry: "reg.example/ns"}); got != "reg.example/ns/svc:alpha-0.1.0" {
		t.Errorf("registry only: got %q", got)
	}
	// An explicit image still wins over both.
	if got := b.imageFor(workloadSpec{Service: "svc", Registry: "reg.example/ns", Tag: "t", Image: "x:1"}); got != "x:1" {
		t.Errorf("explicit image: got %q", got)
	}
}

func TestValidateImageRequest_Registry(t *testing.T) {
	cases := []struct {
		name    string
		reg     string
		wantErr bool
	}{
		{"host and port and path", "192.168.53.171:3000/jp01", false},
		{"plain host", "ghcr.io/code-armory-app", false},
		{"clearing it", "", false},
		// A scheme would compose to "https:/host/repo:tag"; a trailing slash doubles up.
		{"with scheme", "https://192.168.53.171:3000/jp01", true},
		{"trailing slash", "192.168.53.171:3000/jp01/", true},
		{"leading slash", "/jp01", true},
		{"whitespace", "reg .io/ns", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.reg
			if msg := validateImageRequest(setImageRequest{Registry: &r}); (msg != "") != c.wantErr {
				t.Errorf("registry %q -> %q, wantErr=%v", c.reg, msg, c.wantErr)
			}
		})
	}
}
