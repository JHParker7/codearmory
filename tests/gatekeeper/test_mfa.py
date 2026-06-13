"""
Integration tests for TOTP MFA — covers the full lifecycle:
  enroll → confirm → login (challenge) → verify → disable

Also covers the OAuth TOTP flow:
  oauth/authorize (with TOTP user) → oauth/mfa GET → oauth/mfa POST → redirect with code
"""
import uuid
from datetime import datetime, timedelta

import pyotp
import requests


# ── Helpers ───────────────────────────────────────────────────────────────────


def wrong_code(secret):
    """Return a 6-digit code that is deliberately invalid for `secret` right now.

    Using a hardcoded constant like "000000" occasionally collides with a genuinely
    valid code and flakes. Generating a code for a time step well outside the
    server's acceptance window guarantees rejection. As a safety net (the skewed
    code could, with vanishing probability, equal the current one), mutate a digit
    if it happens to match the live code.
    """
    totp = pyotp.TOTP(secret)
    code = totp.at(datetime.now() - timedelta(seconds=90))
    if code == totp.now():
        code = ("1" if code[0] != "1" else "2") + code[1:]
    return code


def signup_and_login(base_url, *, password="password123"):
    """Create a fresh user via /signup and return (token, user payload)."""
    uid = uuid.uuid4().hex[:8]
    payload = {
        "email": f"mfa_{uid}@example.com",
        "username": f"mfa_{uid}",
        "password": password,
    }
    resp = requests.post(f"{base_url}/signup", json=payload)
    assert resp.status_code == 201, f"signup failed: {resp.text}"
    payload["user_id"] = resp.json()["user_id"]

    login = requests.post(
        f"{base_url}/login",
        json={"email": payload["email"], "password": password},
    )
    assert login.status_code == 200, f"login failed: {login.text}"
    payload["token"] = login.json()["token"]
    return payload


def enroll_and_confirm_totp(base_url, token):
    """Enroll TOTP for the authenticated user and confirm with the first valid code.

    Returns the plaintext TOTP secret so the caller can generate further codes.
    """
    enroll = requests.post(
        f"{base_url}/mfa/totp/enroll",
        headers={"Authorization": f"Bearer {token}"},
    )
    assert enroll.status_code == 200, f"enroll failed: {enroll.text}"
    secret = enroll.json()["secret"]

    code = pyotp.TOTP(secret).now()
    confirm = requests.post(
        f"{base_url}/mfa/totp/confirm",
        json={"code": code},
        headers={"Authorization": f"Bearer {token}"},
    )
    assert confirm.status_code == 204, f"confirm failed: {confirm.text}"
    return secret


# ── TestTOTPEnroll ────────────────────────────────────────────────────────────


class TestTOTPEnroll:
    def test_returns_200_with_uri_and_secret(self, base_url, token):
        resp = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "uri" in body and body["uri"].startswith("otpauth://")
        assert "secret" in body and body["secret"] != ""

    def test_uri_is_valid_otpauth(self, base_url, token):
        resp = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        # pyotp can parse a well-formed provisioning URI
        totp = pyotp.parse_uri(resp.json()["uri"])
        assert totp is not None

    def test_secret_generates_valid_codes(self, base_url, token):
        resp = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        secret = resp.json()["secret"]
        code = pyotp.TOTP(secret).now()
        assert len(code) == 6 and code.isdigit()

    def test_requires_auth(self, base_url):
        resp = requests.post(f"{base_url}/mfa/totp/enroll")
        assert resp.status_code == 401

    def test_conflict_when_already_enabled(self, base_url):
        user = signup_and_login(base_url)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert resp.status_code == 409

    def test_reenroll_replaces_unconfirmed(self, base_url, token):
        # First enroll (unconfirmed)
        r1 = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert r1.status_code == 200
        secret1 = r1.json()["secret"]

        # Second enroll should succeed and replace the first
        r2 = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert r2.status_code == 200
        secret2 = r2.json()["secret"]
        assert secret1 != secret2


# ── TestTOTPConfirm ───────────────────────────────────────────────────────────


class TestTOTPConfirm:
    def test_returns_204_with_valid_code(self, base_url, token):
        enroll = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert enroll.status_code == 200
        secret = enroll.json()["secret"]

        resp = requests.post(
            f"{base_url}/mfa/totp/confirm",
            json={"code": pyotp.TOTP(secret).now()},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 204

    def test_401_for_wrong_code(self, base_url, token):
        enroll = requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert enroll.status_code == 200
        secret = enroll.json()["secret"]
        resp = requests.post(
            f"{base_url}/mfa/totp/confirm",
            json={"code": wrong_code(secret)},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 401

    def test_404_with_no_pending_enrollment(self, base_url, token):
        resp = requests.post(
            f"{base_url}/mfa/totp/confirm",
            json={"code": "123456"},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 404

    def test_400_for_missing_code(self, base_url, token):
        requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        resp = requests.post(
            f"{base_url}/mfa/totp/confirm",
            json={},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 400

    def test_requires_auth(self, base_url):
        resp = requests.post(
            f"{base_url}/mfa/totp/confirm", json={"code": "123456"}
        )
        assert resp.status_code == 401


# ── TestTOTPStatus ────────────────────────────────────────────────────────────


class TestTOTPStatus:
    def test_enabled_false_before_enroll(self, base_url, token):
        resp = requests.get(
            f"{base_url}/mfa/totp/status",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200
        assert resp.json()["enabled"] is False

    def test_enabled_false_after_enroll_but_before_confirm(self, base_url, token):
        requests.post(
            f"{base_url}/mfa/totp/enroll",
            headers={"Authorization": f"Bearer {token}"},
        )
        resp = requests.get(
            f"{base_url}/mfa/totp/status",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 200
        assert resp.json()["enabled"] is False

    def test_enabled_true_after_confirm(self, base_url):
        user = signup_and_login(base_url)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.get(
            f"{base_url}/mfa/totp/status",
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert resp.status_code == 200
        assert resp.json()["enabled"] is True

    def test_requires_auth(self, base_url):
        resp = requests.get(f"{base_url}/mfa/totp/status")
        assert resp.status_code == 401


# ── TestTOTPDisable ───────────────────────────────────────────────────────────


class TestTOTPDisable:
    def test_returns_204_with_correct_password(self, base_url):
        password = "disablepass123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.delete(
            f"{base_url}/mfa/totp",
            json={"password": password},
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert resp.status_code == 204

    def test_status_is_disabled_after_disable(self, base_url):
        password = "disablepass123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        requests.delete(
            f"{base_url}/mfa/totp",
            json={"password": password},
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        status = requests.get(
            f"{base_url}/mfa/totp/status",
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert status.json()["enabled"] is False

    def test_401_for_wrong_password(self, base_url):
        user = signup_and_login(base_url)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.delete(
            f"{base_url}/mfa/totp",
            json={"password": "wrongpassword"},
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert resp.status_code == 401

    def test_404_when_not_enrolled(self, base_url, token):
        resp = requests.delete(
            f"{base_url}/mfa/totp",
            json={"password": "password123"},
            headers={"Authorization": f"Bearer {token}"},
        )
        assert resp.status_code == 404

    def test_400_for_missing_password(self, base_url):
        user = signup_and_login(base_url)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.delete(
            f"{base_url}/mfa/totp",
            json={},
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert resp.status_code == 400

    def test_requires_auth(self, base_url):
        resp = requests.delete(
            f"{base_url}/mfa/totp", json={"password": "password123"}
        )
        assert resp.status_code == 401


# ── TestLoginWithTOTP ─────────────────────────────────────────────────────────


class TestLoginWithTOTP:
    def test_login_returns_mfa_challenge(self, base_url):
        password = "loginpass123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        resp = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert body.get("mfa_required") is True
        assert body.get("mfa_token", "") != ""
        assert "token" not in body

    def test_login_returns_token_without_totp(self, base_url, new_user):
        resp = requests.post(
            f"{base_url}/login",
            json={"email": new_user["email"], "password": new_user["password"]},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "token" in body
        assert body.get("mfa_required") is None


# ── TestMFAVerify ─────────────────────────────────────────────────────────────


class TestMFAVerify:
    def test_returns_session_token_for_valid_code(self, base_url):
        password = "verifypass123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        challenge = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        mfa_token = challenge.json()["mfa_token"]

        resp = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": pyotp.TOTP(secret).now()},
        )
        assert resp.status_code == 200
        body = resp.json()
        assert "token" in body
        assert len(body["token"].split(".")) == 3

    def test_issued_token_authenticates_subsequent_requests(self, base_url):
        password = "verifypass123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        challenge = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        mfa_token = challenge.json()["mfa_token"]

        verify = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": pyotp.TOTP(secret).now()},
        )
        session_token = verify.json()["token"]

        me = requests.post(
            f"{base_url}/check_permissions",
            json={"service": "gatekeeper", "resource": "gatekeeper/mfa", "action": "getMFAStatus"},
            headers={"Authorization": f"Bearer {session_token}"},
        )
        assert me.status_code == 200

    def test_401_for_invalid_code(self, base_url):
        password = "verifypass123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        challenge = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        mfa_token = challenge.json()["mfa_token"]

        resp = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": wrong_code(secret)},
        )
        assert resp.status_code == 401

    def test_401_for_unknown_token(self, base_url):
        resp = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": str(uuid.uuid4()), "code": "123456"},
        )
        assert resp.status_code == 401

    def test_token_is_single_use(self, base_url):
        password = "verifypass123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        challenge = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        mfa_token = challenge.json()["mfa_token"]
        code = pyotp.TOTP(secret).now()

        r1 = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": code},
        )
        assert r1.status_code == 200

        r2 = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": code},
        )
        assert r2.status_code == 401

    def test_400_for_missing_mfa_token(self, base_url):
        resp = requests.post(
            f"{base_url}/mfa/verify", json={"code": "123456"}
        )
        assert resp.status_code == 400

    def test_400_for_missing_code(self, base_url):
        resp = requests.post(
            f"{base_url}/mfa/verify", json={"mfa_token": "tok"}
        )
        assert resp.status_code == 400


# ── TestOAuthMFAFlow ──────────────────────────────────────────────────────────


class TestOAuthMFAFlow:
    """End-to-end OAuth2 TOTP flow: authorize → mfa form → verify → code."""

    def test_authorize_redirects_to_mfa_form_when_totp_enabled(self, base_url, service_key, admin_token):
        password = "oauthpass123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        client_resp = requests.post(
            f"{base_url}/internal/oauth/clients",
            json={
                "name": f"mfatest-{uuid.uuid4().hex[:6]}",
                "redirect_uris": ["https://example.com/callback"],
            },
            headers={"X-Service-Key": service_key},
        )
        assert client_resp.status_code == 201, f"failed to create oauth client: {client_resp.text}"

        client = client_resp.json()

        resp = requests.post(
            f"{base_url}/oauth/authorize",
            data={
                "client_id": client["client_id"],
                "redirect_uri": "https://example.com/callback",
                "response_type": "code",
                "state": "teststate",
                "scope": "openid",
                "email": user["email"],
                "password": password,
            },
            allow_redirects=False,
        )
        assert resp.status_code == 302
        location = resp.headers.get("Location", "")
        assert "/oauth/mfa" in location
        assert "token=" in location

    def test_mfa_get_renders_html_form(self, base_url, service_key, admin_token):
        password = "oauthpass123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        client_resp = requests.post(
            f"{base_url}/internal/oauth/clients",
            json={
                "name": f"mfatest-{uuid.uuid4().hex[:6]}",
                "redirect_uris": ["https://example.com/callback"],
            },
            headers={"X-Service-Key": service_key},
        )
        assert client_resp.status_code == 201, f"failed to create oauth client: {client_resp.text}"

        client = client_resp.json()

        authorize = requests.post(
            f"{base_url}/oauth/authorize",
            data={
                "client_id": client["client_id"],
                "redirect_uri": "https://example.com/callback",
                "response_type": "code",
                "state": "teststate",
                "scope": "openid",
                "email": user["email"],
                "password": password,
            },
            allow_redirects=False,
        )
        assert authorize.status_code == 302
        mfa_url = base_url + authorize.headers["Location"].split(base_url, 1)[-1]
        if mfa_url.startswith("/"):
            mfa_url = base_url + mfa_url

        form_resp = requests.get(mfa_url, allow_redirects=False)
        assert form_resp.status_code == 200
        assert "text/html" in form_resp.headers.get("Content-Type", "")
        assert "Authentication code" in form_resp.text or "code" in form_resp.text.lower()

    def test_mfa_post_redirects_with_code_on_valid_totp(self, base_url, service_key, admin_token):
        from urllib.parse import urlparse, parse_qs

        password = "oauthpass123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        client_resp = requests.post(
            f"{base_url}/internal/oauth/clients",
            json={
                "name": f"mfatest-{uuid.uuid4().hex[:6]}",
                "redirect_uris": ["https://example.com/callback"],
            },
            headers={"X-Service-Key": service_key},
        )
        assert client_resp.status_code == 201, f"failed to create oauth client: {client_resp.text}"

        client = client_resp.json()

        authorize = requests.post(
            f"{base_url}/oauth/authorize",
            data={
                "client_id": client["client_id"],
                "redirect_uri": "https://example.com/callback",
                "response_type": "code",
                "state": "mystate",
                "scope": "openid",
                "email": user["email"],
                "password": password,
            },
            allow_redirects=False,
        )
        assert authorize.status_code == 302
        loc = authorize.headers["Location"]
        # Extract token from redirect: /oauth/mfa?token=<tok>
        token_param = dict(p.split("=", 1) for p in loc.split("?", 1)[1].split("&") if "=" in p).get("token", "")
        assert token_param != "", f"no token in Location: {loc}"

        mfa_post = requests.post(
            f"{base_url}/oauth/mfa",
            data={"token": token_param, "code": pyotp.TOTP(secret).now()},
            allow_redirects=False,
        )
        assert mfa_post.status_code == 302
        redirect_loc = mfa_post.headers.get("Location", "")
        parsed = urlparse(redirect_loc)
        params = parse_qs(parsed.query)
        assert "code" in params, f"no code in redirect: {redirect_loc}"
        assert params.get("state", [""])[0] == "mystate"

    def test_mfa_get_400_for_missing_token(self, base_url):
        resp = requests.get(f"{base_url}/oauth/mfa", allow_redirects=False)
        assert resp.status_code == 400

    def test_mfa_get_401_for_invalid_token(self, base_url):
        resp = requests.get(
            f"{base_url}/oauth/mfa?token=does-not-exist", allow_redirects=False
        )
        assert resp.status_code == 401

    def test_mfa_post_401_for_invalid_token(self, base_url):
        resp = requests.post(
            f"{base_url}/oauth/mfa",
            data={"token": "no-such-token", "code": "123456"},
            allow_redirects=False,
        )
        assert resp.status_code == 401


# ── TestMFAFullLoginFlow ──────────────────────────────────────────────────────


class TestMFAFullLoginFlow:
    """End-to-end: enroll → login challenge → verify → use token."""

    def test_full_flow_produces_working_session(self, base_url):
        password = "fullflow123"
        user = signup_and_login(base_url, password=password)
        secret = enroll_and_confirm_totp(base_url, user["token"])

        # Login should now return MFA challenge.
        challenge = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        assert challenge.status_code == 200
        assert challenge.json().get("mfa_required") is True
        mfa_token = challenge.json()["mfa_token"]

        # Verify produces a real session.
        verify = requests.post(
            f"{base_url}/mfa/verify",
            json={"mfa_token": mfa_token, "code": pyotp.TOTP(secret).now()},
        )
        assert verify.status_code == 200
        session_token = verify.json()["token"]
        assert len(session_token.split(".")) == 3

        # Session token grants access to protected endpoints.
        status = requests.get(
            f"{base_url}/mfa/totp/status",
            headers={"Authorization": f"Bearer {session_token}"},
        )
        assert status.status_code == 200
        assert status.json()["enabled"] is True

    def test_disable_then_login_returns_token_directly(self, base_url):
        password = "toggleflow123"
        user = signup_and_login(base_url, password=password)
        enroll_and_confirm_totp(base_url, user["token"])

        # Disable TOTP.
        disable = requests.delete(
            f"{base_url}/mfa/totp",
            json={"password": password},
            headers={"Authorization": f"Bearer {user['token']}"},
        )
        assert disable.status_code == 204

        # Login should now return a session token directly.
        login = requests.post(
            f"{base_url}/login",
            json={"email": user["email"], "password": password},
        )
        assert login.status_code == 200
        assert "token" in login.json()
        assert login.json().get("mfa_required") is None
