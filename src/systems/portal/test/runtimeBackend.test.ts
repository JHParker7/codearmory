import { expect } from 'chai';
import { RUNTIME_TYPES, requiresRuntimeClass, parseKVLines, formatKVLines } from '../src/pages/app/runtimeBackend.ts';

describe('RUNTIME_TYPES', () => {
  it('is exactly the four forge runtime implementations', () => {
    expect([...RUNTIME_TYPES]).to.deep.equal(['docker', 'kubernetes', 'kata', 'gvisor']);
  });
});

describe('requiresRuntimeClass', () => {
  it('is true for the kernel-isolated backends (need a RuntimeClass)', () => {
    expect(requiresRuntimeClass('kata')).to.equal(true);
    expect(requiresRuntimeClass('gvisor')).to.equal(true);
  });
  it('is false for the shared-kernel container backends', () => {
    expect(requiresRuntimeClass('docker')).to.equal(false);
    expect(requiresRuntimeClass('kubernetes')).to.equal(false);
    expect(requiresRuntimeClass('')).to.equal(false);
  });
});

describe('parseKVLines', () => {
  it('parses key=value lines into a map', () => {
    expect(parseKVLines('runtime_class=kata-qemu\nfoo=bar')).to.deep.equal({ runtime_class: 'kata-qemu', foo: 'bar' });
  });
  it('skips blank lines and trims whitespace', () => {
    expect(parseKVLines('\n  a = 1 \n\n b=2\n')).to.deep.equal({ a: '1', b: '2' });
  });
  it('keeps only the first = as the split, so values may contain =', () => {
    expect(parseKVLines('url=https://h/x?a=b')).to.deep.equal({ url: 'https://h/x?a=b' });
  });
  it('returns an empty object for empty/whitespace input', () => {
    expect(parseKVLines('')).to.deep.equal({});
    expect(parseKVLines('   \n  ')).to.deep.equal({});
  });
  it('throws on a line missing =', () => {
    expect(() => parseKVLines('runtime_class')).to.throw(/key=value/);
  });
  it('throws on an empty key', () => {
    expect(() => parseKVLines('=value')).to.throw(/empty key/);
  });
});

describe('formatKVLines', () => {
  it('renders a map to sorted key=value lines', () => {
    expect(formatKVLines({ b: '2', a: '1' })).to.equal('a=1\nb=2');
  });
  it('returns an empty string for null/undefined/empty', () => {
    expect(formatKVLines(null)).to.equal('');
    expect(formatKVLines(undefined)).to.equal('');
    expect(formatKVLines({})).to.equal('');
  });
  it('round-trips with parseKVLines', () => {
    const m = { runtime_class: 'gvisor', region: 'us' };
    expect(parseKVLines(formatKVLines(m))).to.deep.equal(m);
  });
});
