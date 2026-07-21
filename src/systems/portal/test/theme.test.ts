import { expect } from 'chai';
import { oklchToHex, THEMES } from '../src/theme';

describe('oklchToHex', () => {
  it('converts the default cyber accent to its sRGB hex', () => {
    expect(oklchToHex('78% 0.16 145')).to.equal('#6ed274');
  });

  it('handles the achromatic ends', () => {
    expect(oklchToHex('0% 0 0')).to.equal('#000000');
    expect(oklchToHex('100% 0 0')).to.equal('#ffffff');
  });

  it('accepts a unitless lightness', () => {
    expect(oklchToHex('0.78 0.16 145')).to.equal(oklchToHex('78% 0.16 145'));
  });

  it('clamps out-of-gamut colours instead of emitting a malformed hex', () => {
    // A chroma no sRGB display can reach — the channels must still land in range, since
    // a favicon whose colour the browser cannot parse renders as nothing at all.
    expect(oklchToHex('78% 0.9 145')).to.match(/^#[0-9a-f]{6}$/);
  });

  it('resolves every shipped theme accent to a valid hex', () => {
    for (const t of THEMES) {
      const token = t.accent.replace(/^oklch\(|\)$/g, '');
      expect(oklchToHex(token), t.name).to.match(/^#[0-9a-f]{6}$/);
    }
  });
});
