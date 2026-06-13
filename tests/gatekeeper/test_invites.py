"""Integration tests for the invite lifecycle.

Admin (session-scoped) is always the inviter — it holds wildcard permissions so
invite creation succeeds without extra DB wiring.  new_user (function-scoped) is
always the invitee so each test gets a clean, uncontaminated target.

Org invite tests create their own org via a fixture and delete it on teardown.
Team invite tests create their own team the same way.
"""

import uuid
import pytest
import requests


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

AUTH_HEADER = "Authorization"


def bearer(token):
    return {AUTH_HEADER: f"Bearer {token}"}


def rand_id():
    return uuid.uuid4().hex


def _login(base_url, email, password):
    """Return a JWT for the given credentials."""
    resp = requests.post(
        f"{base_url}/login",
        json={"email": email, "password": password},
    )
    assert resp.status_code == 200, f"login failed for {email}: {resp.text}"
    return resp.json()["token"]


# ---------------------------------------------------------------------------
# Org invites
# ---------------------------------------------------------------------------


class TestOrgInvite:
    @pytest.fixture
    def org(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": f"invite-org-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"org create failed: {resp.text}"
        data = resp.json()

        yield data

        requests.delete(
            f"{base_url}/orgs/{data['org_id']}",
            headers=bearer(admin_token["token"]),
        )

    @pytest.fixture
    def org_invite(self, base_url, admin_token, new_user, org):
        """Create an org invite and return the response body."""
        resp = requests.post(
            f"{base_url}/orgs/{org['org_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"org invite create failed: {resp.text}"
        return resp.json()

    def test_create_returns_201(self, base_url, admin_token, new_user, org):
        resp = requests.post(
            f"{base_url}/orgs/{org['org_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201

    def test_create_response_has_invite_id(self, base_url, admin_token, new_user, org):
        resp = requests.post(
            f"{base_url}/orgs/{org['org_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201
        body = resp.json()
        assert "invite_id" in body
        assert body["invite_id"] != ""

    def test_inviter_can_get_invite(
        self, base_url, admin_token, org_invite
    ):
        resp = requests.get(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200

    def test_invitee_can_get_invite(
        self, base_url, new_user, org_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        resp = requests.get(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(invitee_token),
        )
        assert resp.status_code == 200

    def test_third_party_cannot_get_invite(
        self, base_url, admin_token, new_user, org_invite
    ):
        # Create a third user who is neither inviter nor invitee
        uid = uuid.uuid4().hex[:8]
        requests.post(
            f"{base_url}/signup",
            json={
                "email": f"third_{uid}@example.com",
                "username": f"third_{uid}",
                "password": "password123",
            },
        )
        third_token = _login(base_url, f"third_{uid}@example.com", "password123")

        resp = requests.get(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(third_token),
        )
        # Privacy: must not reveal invite existence to unrelated parties
        assert resp.status_code == 404

    def test_accept_invite_returns_204(
        self, base_url, new_user, org_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        resp = requests.post(
            f"{base_url}/invites/{org_invite['invite_id']}/accept",
            headers=bearer(invitee_token),
        )
        assert resp.status_code == 204

    def test_decline_invite_returns_204(
        self, base_url, new_user, org_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        resp = requests.post(
            f"{base_url}/invites/{org_invite['invite_id']}/decline",
            headers=bearer(invitee_token),
        )
        assert resp.status_code == 204

    def test_invitee_cannot_accept_already_declined(
        self, base_url, new_user, org_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        requests.post(
            f"{base_url}/invites/{org_invite['invite_id']}/decline",
            headers=bearer(invitee_token),
        )
        resp = requests.post(
            f"{base_url}/invites/{org_invite['invite_id']}/accept",
            headers=bearer(invitee_token),
        )
        # handleAcceptInvite rejects a non-pending invite with 409 "invite is not pending".
        assert resp.status_code == 409

    def test_only_invitee_can_accept(
        self, base_url, admin_token, org_invite
    ):
        # The inviter (admin) must not be able to accept their own invite
        resp = requests.post(
            f"{base_url}/invites/{org_invite['invite_id']}/accept",
            headers=bearer(admin_token["token"]),
        )
        # The inviter's email != the invitee email, so handleAcceptInvite returns 403.
        assert resp.status_code == 403

    def test_inviter_can_delete_invite(
        self, base_url, admin_token, org_invite
    ):
        resp = requests.delete(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 204

    def test_deleted_invite_returns_404_on_get(
        self, base_url, admin_token, org_invite
    ):
        requests.delete(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(admin_token["token"]),
        )
        resp = requests.get(
            f"{base_url}/invites/{org_invite['invite_id']}",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Team invites
# ---------------------------------------------------------------------------


class TestTeamInvite:
    @pytest.fixture
    def team(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/teams",
            json={"team_name": f"invite-team-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"team create failed: {resp.text}"
        data = resp.json()

        yield data

        requests.delete(
            f"{base_url}/teams/{data['team_id']}",
            headers=bearer(admin_token["token"]),
        )

    @pytest.fixture
    def team_invite(self, base_url, admin_token, new_user, team):
        """Create a team invite and return the response body."""
        resp = requests.post(
            f"{base_url}/teams/{team['team_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"team invite create failed: {resp.text}"
        return resp.json()

    def test_create_returns_201(self, base_url, admin_token, new_user, team):
        resp = requests.post(
            f"{base_url}/teams/{team['team_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201

    def test_invitee_can_get_invite(
        self, base_url, new_user, team_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        resp = requests.get(
            f"{base_url}/invites/{team_invite['invite_id']}",
            headers=bearer(invitee_token),
        )
        assert resp.status_code == 200

    def test_accept_invite_returns_204(
        self, base_url, new_user, team_invite
    ):
        invitee_token = _login(base_url, new_user["email"], new_user["password"])
        resp = requests.post(
            f"{base_url}/invites/{team_invite['invite_id']}/accept",
            headers=bearer(invitee_token),
        )
        assert resp.status_code == 204


# ---------------------------------------------------------------------------
# Invite list
# ---------------------------------------------------------------------------


class TestInviteList:
    @pytest.fixture
    def org(self, base_url, admin_token):
        resp = requests.post(
            f"{base_url}/orgs",
            json={"org_name": f"list-org-{rand_id()}"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"org create failed: {resp.text}"
        data = resp.json()

        yield data

        requests.delete(
            f"{base_url}/orgs/{data['org_id']}",
            headers=bearer(admin_token["token"]),
        )

    @pytest.fixture
    def sent_invite(self, base_url, admin_token, new_user, org):
        """Create an org invite so there is at least one sent invite for the admin."""
        resp = requests.post(
            f"{base_url}/orgs/{org['org_id']}/invites",
            json={"email": new_user["email"]},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 201, f"invite create failed: {resp.text}"
        return resp.json()

    def test_list_returns_array(self, base_url, admin_token, sent_invite):
        resp = requests.get(
            f"{base_url}/invites",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        assert isinstance(resp.json(), list)

    def test_list_filter_by_status_pending(self, base_url, admin_token, sent_invite):
        resp = requests.get(
            f"{base_url}/invites",
            params={"status": "pending"},
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        body = resp.json()
        assert isinstance(body, list)
        for invite in body:
            assert invite.get("status") == "pending"

    def test_list_shows_sent_invites(self, base_url, admin_token, sent_invite):
        resp = requests.get(
            f"{base_url}/invites",
            headers=bearer(admin_token["token"]),
        )
        assert resp.status_code == 200
        invite_ids = [i["invite_id"] for i in resp.json()]
        assert sent_invite["invite_id"] in invite_ids
