package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestSeedPlatformAccount_IsIdempotent — it runs on every startup, so a second call
// must be a no-op rather than a duplicate-key error or a second row.
func TestSeedPlatformAccount_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	seedPlatformAccount(ctx)
	seedPlatformAccount(ctx)

	var n int64
	if err := connect().Model(&User{}).Where("user_id = ?", platformUserID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("platform account row count = %d, want exactly 1", n)
	}
}

// TestSeedPlatformAccount_IsResolvableForAttribution is the reason the row exists at
// all: a platform-config change must attribute to a subject that resolves, rather than
// to an id pointing at nothing.
func TestSeedPlatformAccount_IsResolvableForAttribution(t *testing.T) {
	ctx := context.Background()
	seedPlatformAccount(ctx)

	row, err := (User{UserID: platformUserID}).Get(ctx)
	if err != nil {
		t.Fatalf("platform account does not resolve: %v", err)
	}
	u := row.(User)
	if u.Username != platformUsername {
		t.Errorf("username = %q, want %q — it must match the namespace it owns", u.Username, platformUsername)
	}
	if !isPlatformAccount(u) {
		t.Error("isPlatformAccount is false for the seeded row")
	}
}

// TestPlatformAccount_PasswordCanNeverVerify pins the stored credential itself. The
// explicit isPlatformAccount guard in the login paths is the primary rule; this proves
// the account is ALSO unusable by construction, so a future auth path that forgets the
// guard still cannot authenticate it.
func TestPlatformAccount_PasswordCanNeverVerify(t *testing.T) {
	ctx := context.Background()
	seedPlatformAccount(ctx)

	row, err := (User{UserID: platformUserID}).Get(ctx)
	if err != nil {
		t.Fatalf("get platform account: %v", err)
	}
	stored := row.(User).HashedPassword
	if stored != lockedPasswordHash {
		t.Fatalf("stored hash = %q, want the locked sentinel %q", stored, lockedPasswordHash)
	}
	// bcrypt errors on a malformed hash rather than matching, so nothing verifies —
	// including the sentinel itself, and including an empty password.
	for _, guess := range []string{"", lockedPasswordHash, "password", "codearmory"} {
		if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(guess)); err == nil {
			t.Errorf("password %q verified against the locked account", guess)
		}
	}
}

// TestPlatformAccount_IdentifiedByIDNotName — renaming or re-addressing the row must
// not turn it back into a loginable account, so the guard keys on the fixed id.
func TestPlatformAccount_IdentifiedByIDNotName(t *testing.T) {
	renamed := User{UserID: platformUserID, Username: "someone-else", Email: "x@y.test"}
	if !isPlatformAccount(renamed) {
		t.Error("a renamed platform account stopped being recognised — the guard must key on the id")
	}
	impostor := User{UserID: "11111111-1111-1111-1111-111111111111", Username: platformUsername}
	if isPlatformAccount(impostor) {
		t.Error("a different row claiming the name was treated as the platform account")
	}
}

// TestPlatformAccount_NameStaysReserved ties this to the other half: the namespace is
// only an isolation boundary because no ordinary user can be created under that name.
func TestPlatformAccount_NameStaysReserved(t *testing.T) {
	if err := checkReservedUsername(platformUsername); err == nil {
		t.Fatal("the platform username is not reserved — a user could claim the namespace it owns")
	} else if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error = %v, want it to say the name is reserved", err)
	}
}

// TestHandleLogin_PlatformAccountIsRefused is the property that actually matters: the
// login HANDLER refuses it, end to end. A locked hash and a guard helper are both
// worthless if the handler never consults them.
//
// It also checks the response is indistinguishable from an unknown email — same status,
// no body hinting the account exists — so the platform account cannot be discovered by
// probing the login endpoint.
func TestHandleLogin_PlatformAccountIsRefused(t *testing.T) {
	seedPlatformAccount(context.Background())

	// Every password shape someone might try, including the sentinel itself.
	for _, guess := range []string{"", lockedPasswordHash, "codearmory", "password"} {
		b, _ := json.Marshal(loginRequest{Email: platformEmail, Password: guess})
		r := httptest.NewRequest(http.MethodPost, "/login", bytes.NewReader(b))
		w := httptest.NewRecorder()
		handleLogin(w, r)

		if w.Code == http.StatusOK {
			t.Fatalf("password %q logged into the platform account: %s", guess, w.Body.String())
		}
		if guess != "" && w.Code != http.StatusUnauthorized {
			t.Errorf("password %q gave status %d, want 401", guess, w.Code)
		}
		if strings.Contains(strings.ToLower(w.Body.String()), "platform") {
			t.Errorf("response body reveals the account: %q", w.Body.String())
		}
	}

	// And no session was minted along the way.
	var sessions int64
	if err := connect().Model(&Session{}).Where("user_id = ?", platformUserID).Count(&sessions).Error; err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("platform account has %d sessions, want 0", sessions)
	}
}
