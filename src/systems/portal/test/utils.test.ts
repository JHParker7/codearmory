import { expect } from 'chai';
import { timeAgo, passwordScore, decodeUserId, parseStoredWorkspaces } from '../src/utils.ts';

// ---------------------------------------------------------------------------
// timeAgo
// ---------------------------------------------------------------------------

describe('timeAgo', () => {
  const base = new Date('2024-01-01T12:00:00Z').getTime();

  it('returns seconds for sub-minute diffs', () => {
    expect(timeAgo(new Date(base - 45_000).toISOString(), base)).to.equal('45s');
  });

  it('returns minutes for sub-hour diffs', () => {
    expect(timeAgo(new Date(base - 5 * 60_000).toISOString(), base)).to.equal('5m');
  });

  it('returns hours for sub-day diffs', () => {
    expect(timeAgo(new Date(base - 3 * 3_600_000).toISOString(), base)).to.equal('3h');
  });

  it('returns days for multi-day diffs', () => {
    expect(timeAgo(new Date(base - 2 * 86_400_000).toISOString(), base)).to.equal('2d');
  });

  it('returns 0s for the exact same timestamp', () => {
    expect(timeAgo(new Date(base).toISOString(), base)).to.equal('0s');
  });

  it('returns 59s at the minute boundary', () => {
    expect(timeAgo(new Date(base - 59_000).toISOString(), base)).to.equal('59s');
  });

  it('returns 1m at exactly 60 seconds', () => {
    expect(timeAgo(new Date(base - 60_000).toISOString(), base)).to.equal('1m');
  });

  it('returns 23h just under a day', () => {
    expect(timeAgo(new Date(base - 23 * 3_600_000).toISOString(), base)).to.equal('23h');
  });
});

// ---------------------------------------------------------------------------
// passwordScore
// ---------------------------------------------------------------------------

describe('passwordScore', () => {
  it('scores empty string as 0', () => {
    const { score, checks } = passwordScore('');
    expect(score).to.equal(0);
    expect(checks.len).to.be.false;
  });

  it('scores a password with all 5 checks', () => {
    const { score, checks } = passwordScore('Abcdef1!');
    expect(score).to.equal(5);
    expect(checks).to.deep.equal({ len: true, upper: true, lower: true, num: true, sym: true });
  });

  it('fails len check for passwords shorter than 8 chars', () => {
    const { checks } = passwordScore('Ab1!');
    expect(checks.len).to.be.false;
    expect(checks.upper).to.be.true;
    expect(checks.sym).to.be.true;
  });

  it('fails upper check when no uppercase', () => {
    expect(passwordScore('abcdef1!').checks.upper).to.be.false;
  });

  it('fails lower check when no lowercase', () => {
    expect(passwordScore('ABCDEF1!').checks.lower).to.be.false;
  });

  it('fails num check when no digit', () => {
    expect(passwordScore('Abcdefg!').checks.num).to.be.false;
  });

  it('fails sym check when only alphanumeric', () => {
    expect(passwordScore('Abcdefg1').checks.sym).to.be.false;
  });

  it('scores "password" as 2 (lower + len)', () => {
    const { score, checks } = passwordScore('password');
    expect(score).to.equal(2);
    expect(checks.lower).to.be.true;
    expect(checks.len).to.be.true;
    expect(checks.upper).to.be.false;
  });
});

// ---------------------------------------------------------------------------
// decodeUserId
// ---------------------------------------------------------------------------

function makeJwt(payload: Record<string, unknown>): string {
  const encoded = Buffer.from(JSON.stringify(payload)).toString('base64url');
  return `header.${encoded}.signature`;
}

describe('decodeUserId', () => {
  it('extracts the sub claim from a valid JWT', () => {
    const token = makeJwt({ sub: 'user-uuid-123', exp: 9999999999 });
    expect(decodeUserId(token)).to.equal('user-uuid-123');
  });

  it('returns null when sub is not a string', () => {
    expect(decodeUserId(makeJwt({ sub: 42 }))).to.be.null;
  });

  it('returns null when sub is missing', () => {
    expect(decodeUserId(makeJwt({ iss: 'gatekeeper' }))).to.be.null;
  });

  it('returns null for a completely invalid token', () => {
    expect(decodeUserId('not.a.token')).to.be.null;
  });

  it('returns null for an empty string', () => {
    expect(decodeUserId('')).to.be.null;
  });

  it('returns null for a one-part token', () => {
    expect(decodeUserId('onlyonepart')).to.be.null;
  });

  it('handles URL-safe base64 characters (- and _)', () => {
    // Craft a payload whose base64url encoding contains - and _
    const payload = { sub: 'user-with-special_chars' };
    const encoded = Buffer.from(JSON.stringify(payload)).toString('base64url');
    const token = `h.${encoded}.s`;
    expect(decodeUserId(token)).to.equal('user-with-special_chars');
  });
});

// ---------------------------------------------------------------------------
// parseStoredWorkspaces
// ---------------------------------------------------------------------------

describe('parseStoredWorkspaces', () => {
  it('parses a valid array of workspace entries', () => {
    const raw = JSON.stringify([
      { path: 'alice/prod', label: 'alice/prod' },
      { path: 'acme/platform/staging', label: 'acme/platform/staging' },
    ]);
    const result = parseStoredWorkspaces(raw);
    expect(result).to.have.lengthOf(2);
    expect(result[0].path).to.equal('alice/prod');
  });

  it('returns an empty array for an empty JSON array', () => {
    expect(parseStoredWorkspaces('[]')).to.deep.equal([]);
  });

  it('returns empty array for invalid JSON', () => {
    expect(parseStoredWorkspaces('not json')).to.deep.equal([]);
  });

  it('returns empty array when entries are missing path', () => {
    const raw = JSON.stringify([{ label: 'only-label' }]);
    expect(parseStoredWorkspaces(raw)).to.deep.equal([]);
  });

  it('returns empty array when entries are missing label', () => {
    const raw = JSON.stringify([{ path: 'alice/prod' }]);
    expect(parseStoredWorkspaces(raw)).to.deep.equal([]);
  });

  it('returns empty array when data is not an array', () => {
    expect(parseStoredWorkspaces(JSON.stringify({ path: 'alice/prod', label: 'x' }))).to.deep.equal([]);
  });

  it('returns empty array for an array of non-objects', () => {
    expect(parseStoredWorkspaces(JSON.stringify(['alice/prod']))).to.deep.equal([]);
  });

  it('rejects partially invalid entries — all-or-nothing', () => {
    const raw = JSON.stringify([
      { path: 'alice/prod', label: 'alice/prod' },
      { label: 'missing-path' },
    ]);
    expect(parseStoredWorkspaces(raw)).to.deep.equal([]);
  });

  it('handles null gracefully', () => {
    expect(parseStoredWorkspaces(JSON.stringify(null))).to.deep.equal([]);
  });
});
