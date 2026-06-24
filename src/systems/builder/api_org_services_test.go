package main

import (
	"reflect"
	"testing"
)

func TestMissingRequiredConfig(t *testing.T) {
	withSecretsKey(t) // enable encryption so the existing-secrets branch can decrypt

	blueprints, _ := embeddedServiceDef("blueprints") // requires DATABASE_URL; REDIS_URL optional (managed when absent)
	containers, _ := embeddedServiceDef("containers")  // requires REGISTRY_URL

	storedRedis, err := encryptSecretsMap(map[string]string{"REDIS_URL": "redis://r:6379"}, "blueprints")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	tests := []struct {
		name    string
		def     serviceDef
		req     setServiceRequest
		exist   OrgService
		service string
		want    []string
	}{
		{
			// REDIS_URL is no longer required — builder provisions an in-cluster Redis when
			// it is absent — so only DATABASE_URL gates enable.
			name:    "blueprints missing db only",
			def:     blueprints,
			req:     setServiceRequest{},
			service: "blueprints",
			want:    []string{"DATABASE_URL"},
		},
		{
			// db_url supplied and no REDIS_URL: satisfied, since Redis is now managed.
			name:    "blueprints db only is satisfied (redis managed)",
			def:     blueprints,
			req:     setServiceRequest{DBUrl: "postgres://b@db/b"},
			service: "blueprints",
			want:    nil,
		},
		{
			name:    "blueprints satisfied via db_url + secrets",
			def:     blueprints,
			req:     setServiceRequest{DBUrl: "postgres://b@db/b", Secrets: map[string]string{"REDIS_URL": "redis://r"}},
			service: "blueprints",
			want:    nil,
		},
		{
			name:    "blueprints satisfied via stored db + stored secrets",
			def:     blueprints,
			req:     setServiceRequest{},
			exist:   OrgService{DBURLCiphertext: []byte("x"), SecretsCiphertext: storedRedis},
			service: "blueprints",
			want:    nil,
		},
		{
			name:    "containers needs registry-url (plain config)",
			def:     containers,
			req:     setServiceRequest{Config: map[string]any{"REGISTRY_URL": "https://reg"}},
			service: "containers",
			want:    nil,
		},
		{
			name:    "containers missing registry-url",
			def:     containers,
			req:     setServiceRequest{},
			service: "containers",
			want:    []string{"REGISTRY_URL"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := missingRequiredConfig(tc.def, tc.req, tc.exist, tc.service)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missing = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSecretsMap_RoundTrip(t *testing.T) {
	withSecretsKey(t)
	in := map[string]string{"REDIS_URL": "redis://r:6379", "GITEA_ADMIN_TOKEN": "tok"}

	ct, err := encryptSecretsMap(in, "gitea_integration")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	out, err := decryptSecretsMap(ct, "gitea_integration")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch: %v vs %v", in, out)
	}
	// AAD binds the ciphertext to the service: decrypting as another service fails.
	if _, err := decryptSecretsMap(ct, "forge"); err == nil {
		t.Error("expected decrypt under a different service to fail (AAD mismatch)")
	}
	// Empty ciphertext is a clean no-op.
	if m, err := decryptSecretsMap(nil, "x"); err != nil || m != nil {
		t.Errorf("empty ct: got (%v,%v), want (nil,nil)", m, err)
	}
}
