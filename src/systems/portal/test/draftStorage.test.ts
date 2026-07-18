import { expect } from 'chai';
import { loadDraft, saveDraft, clearDraft } from '../src/draftStorage.js';

/** Minimal in-memory localStorage stand-in; `failOn` forces the named op to throw. */
function fakeStorage(failOn?: 'get' | 'set' | 'remove') {
  const map = new Map<string, string>();
  return {
    getItem(k: string) { if (failOn === 'get') throw new Error('unavailable'); return map.has(k) ? map.get(k)! : null; },
    setItem(k: string, v: string) { if (failOn === 'set') throw new Error('quota'); map.set(k, v); },
    removeItem(k: string) { if (failOn === 'remove') throw new Error('unavailable'); map.delete(k); },
    _map: map,
  };
}

describe('draftStorage', () => {
  const g = globalThis as unknown as { localStorage?: unknown };
  let original: unknown;
  beforeEach(() => { original = g.localStorage; });
  afterEach(() => { g.localStorage = original; });

  it('round-trips a saved draft', () => {
    g.localStorage = fakeStorage();
    saveDraft('k', 'hello');
    expect(loadDraft('k')).to.equal('hello');
  });

  it('returns null for a key that was never saved', () => {
    g.localStorage = fakeStorage();
    expect(loadDraft('missing')).to.equal(null);
  });

  it('clearDraft removes a saved draft', () => {
    g.localStorage = fakeStorage();
    saveDraft('k', 'hello');
    clearDraft('k');
    expect(loadDraft('k')).to.equal(null);
  });

  it('loadDraft returns null instead of throwing when storage is unavailable', () => {
    g.localStorage = fakeStorage('get');
    expect(loadDraft('k')).to.equal(null);
  });

  it('saveDraft silently no-ops when storage throws (e.g. quota exceeded)', () => {
    g.localStorage = fakeStorage('set');
    expect(() => saveDraft('k', 'v')).to.not.throw();
  });

  it('clearDraft silently no-ops when storage throws', () => {
    g.localStorage = fakeStorage('remove');
    expect(() => clearDraft('k')).to.not.throw();
  });

  it('keeps drafts under distinct keys isolated', () => {
    g.localStorage = fakeStorage();
    saveDraft('ci.pipeline.draft:new', 'A');
    saveDraft('ci.pipeline.draft:wf-1', 'B');
    expect(loadDraft('ci.pipeline.draft:new')).to.equal('A');
    expect(loadDraft('ci.pipeline.draft:wf-1')).to.equal('B');
    clearDraft('ci.pipeline.draft:new');
    expect(loadDraft('ci.pipeline.draft:new')).to.equal(null);
    expect(loadDraft('ci.pipeline.draft:wf-1')).to.equal('B');
  });
});
