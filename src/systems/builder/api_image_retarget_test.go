package main

import "testing"

// A tag-only retarget (what CI does after pushing a build) must clear any stored
// explicit image. imageFor gives Image strict precedence over Registry+Tag, so a
// leftover full reference silently pins the service to the OLD build: the row records
// the new tag and the API returns 200, but the reconciler keeps deploying the old
// image. Observed live on git_factory (back when builder still deployed it, before it
// moved in-repo as a core service): it sat on :704caa9 while its row said tag=b42dff7.
func TestRetargetByTagClearsExplicitImage(t *testing.T) {
	tag := "b42dff7"
	registry := "192.168.53.171:3000/jp01"

	for _, tc := range []struct {
		name      string
		req       setImageRequest
		wantImage string
	}{
		{
			name:      "tag only clears a stale explicit image",
			req:       setImageRequest{Tag: &tag},
			wantImage: "",
		},
		{
			name:      "registry+tag clears it too",
			req:       setImageRequest{Registry: &registry, Tag: &tag},
			wantImage: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := OrgService{Image: registry + "/git-factory:704caa9"}
			applyImageRequest(&existing, tc.req)
			if existing.Image != tc.wantImage {
				t.Errorf("Image = %q, want %q — a stale reference outranks the new tag in imageFor", existing.Image, tc.wantImage)
			}
			if tc.req.Tag != nil && existing.Tag != tag {
				t.Errorf("Tag = %q, want %q", existing.Tag, tag)
			}
		})
	}

	// An explicit image IS a deliberate full-reference pin — never discarded, even
	// when a tag rides along.
	t.Run("explicit image is preserved", func(t *testing.T) {
		img := registry + "/git-factory:pinned"
		existing := OrgService{Image: registry + "/git-factory:704caa9"}
		applyImageRequest(&existing, setImageRequest{Image: &img, Tag: &tag})
		if existing.Image != img {
			t.Errorf("Image = %q, want %q — an explicit image must win", existing.Image, img)
		}
	})
}
