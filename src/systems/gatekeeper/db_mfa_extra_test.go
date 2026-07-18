package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTOTPCredential_CRUD(t *testing.T) {
	ctx := context.Background()
	c := TOTPCredential{
		CredentialID: uuid.NewString(), UserID: "u-" + uuid.NewString(), EncSecret: []byte("secret"),
		Confirmed: false, Active: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := c.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { gormDB.Exec(`DELETE FROM totp_credentials WHERE credential_id = ?`, c.CredentialID) }) //nolint:errcheck

	got, err := (TOTPCredential{CredentialID: c.CredentialID}).Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.(TOTPCredential).UserID != c.UserID {
		t.Error("Get returned wrong credential")
	}
	if _, err := (TOTPCredential{UserID: c.UserID}).List(ctx, 10, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
	c.Confirmed = true
	if err := c.Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := c.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestMFAPending_CRUD(t *testing.T) {
	ctx := context.Background()
	m := MFAPending{
		Token: uuid.NewString(), UserID: "u1", ExpiresAt: time.Now().Add(time.Hour).UTC(), CreatedAt: time.Now().UTC(),
	}
	if err := m.Add(ctx); err != nil {
		t.Fatalf("Add: %v", err)
	}
	t.Cleanup(func() { gormDB.Exec(`DELETE FROM mfa_pendings WHERE token = ?`, m.Token) }) //nolint:errcheck
	if _, err := (MFAPending{Token: m.Token}).Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Update/Remove/List are intentional no-ops on this model; exercise them.
	_ = m.Update(ctx)
	_ = m.Remove(ctx)
	if _, err := (MFAPending{}).List(ctx, 10, 0); err != nil {
		t.Errorf("List: %v", err)
	}
}
