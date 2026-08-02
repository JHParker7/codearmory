import { expect } from 'chai';
import {
  parseInline, parseMarkdown, highlight, extOf, splitDiffByFile, diffLineKind, formatBytes,
  type MdInline,
} from '../src/pages/app/repoContent.ts';

/** Flatten an inline tree back to its visible text, for asserting on rendered output. */
function textOf(nodes: MdInline[]): string {
  return nodes.map((n) => (n.kind === 'text' || n.kind === 'code' ? n.text : textOf(n.children))).join('');
}

// ---------------------------------------------------------------------------
// parseInline
// ---------------------------------------------------------------------------

describe('parseInline', () => {
  it('keeps raw HTML as literal text (never markup)', () => {
    const nodes = parseInline('<img src=x onerror=alert(1)>');
    expect(nodes).to.deep.equal([{ kind: 'text', text: '<img src=x onerror=alert(1)>' }]);
  });

  it('renders http links with an href', () => {
    const nodes = parseInline('see [docs](https://example.com/x)');
    const link = nodes.find((n) => n.kind === 'link');
    expect(link).to.exist;
    expect(link && link.kind === 'link' && link.href).to.equal('https://example.com/x');
  });

  it('drops the href of a javascript: link but keeps its text', () => {
    const nodes = parseInline('[click](javascript:alert)');
    expect(nodes.some((n) => n.kind === 'link')).to.equal(false);
    expect(textOf(nodes)).to.equal('click');
  });

  it('drops the href of a relative link, which cannot resolve here', () => {
    const nodes = parseInline('[readme](./README.md)');
    expect(nodes.some((n) => n.kind === 'link')).to.equal(false);
    expect(textOf(nodes)).to.equal('readme');
  });

  it('reduces an image to its alt text — no remote loads', () => {
    expect(textOf(parseInline('![a badge](https://img.example/b.svg)'))).to.equal('a badge');
  });

  it('emphasises with asterisks', () => {
    const nodes = parseInline('**bold** and *italic*');
    expect(nodes.some((n) => n.kind === 'strong')).to.equal(true);
    expect(nodes.some((n) => n.kind === 'em')).to.equal(true);
  });

  it('leaves underscores inside an identifier alone', () => {
    const nodes = parseInline('codearmory_git_factory is one word');
    expect(nodes.every((n) => n.kind === 'text')).to.equal(true);
    expect(textOf(nodes)).to.equal('codearmory_git_factory is one word');
  });

  it('still emphasises underscores at word boundaries', () => {
    const nodes = parseInline('_stressed_ out');
    expect(nodes[0].kind).to.equal('em');
  });

  it('protects code spans from emphasis rules', () => {
    const nodes = parseInline('`a_b_c`');
    expect(nodes).to.deep.equal([{ kind: 'code', text: 'a_b_c' }]);
  });
});

// ---------------------------------------------------------------------------
// parseMarkdown
// ---------------------------------------------------------------------------

describe('parseMarkdown', () => {
  it('parses headings at their level', () => {
    const [h] = parseMarkdown('## Title');
    expect(h.kind).to.equal('heading');
    expect(h.kind === 'heading' && h.level).to.equal(2);
  });

  it('keeps a fenced block verbatim, with its language', () => {
    const [b] = parseMarkdown('```go\nfunc main() {}\n```');
    expect(b.kind).to.equal('code');
    expect(b.kind === 'code' && b.lang).to.equal('go');
    expect(b.kind === 'code' && b.text).to.equal('func main() {}');
  });

  it('does not apply inline rules inside a fenced block', () => {
    const [b] = parseMarkdown('```\n**not bold**\n```');
    expect(b.kind === 'code' && b.text).to.equal('**not bold**');
  });

  it('groups consecutive bullets into one list', () => {
    const [b] = parseMarkdown('- one\n- two\n- three');
    expect(b.kind).to.equal('list');
    expect(b.kind === 'list' && b.ordered).to.equal(false);
    expect(b.kind === 'list' && b.items.length).to.equal(3);
  });

  it('parses an ordered list separately from a bullet list', () => {
    const blocks = parseMarkdown('- a\n\n1. b');
    expect(blocks.map((b) => b.kind)).to.deep.equal(['list', 'list']);
    expect(blocks[1].kind === 'list' && blocks[1].ordered).to.equal(true);
  });

  it('parses a pipe table into head and rows', () => {
    const [b] = parseMarkdown('| a | b |\n|---|---|\n| 1 | 2 |');
    expect(b.kind).to.equal('table');
    expect(b.kind === 'table' && b.head.length).to.equal(2);
    expect(b.kind === 'table' && b.rows.length).to.equal(1);
  });

  it('joins wrapped lines into one paragraph and splits on a blank line', () => {
    const blocks = parseMarkdown('one\ntwo\n\nthree');
    expect(blocks.length).to.equal(2);
    expect(blocks[0].kind === 'paragraph' && textOf(blocks[0].children)).to.equal('one two');
  });

  it('parses a horizontal rule', () => {
    expect(parseMarkdown('---')[0].kind).to.equal('rule');
  });

  it('parses a block quote', () => {
    const [b] = parseMarkdown('> quoted');
    expect(b.kind).to.equal('quote');
  });
});

// ---------------------------------------------------------------------------
// highlight
// ---------------------------------------------------------------------------

describe('highlight', () => {
  it('round-trips the source exactly', () => {
    const src = 'const x = "hi" // note\n';
    expect(highlight(src, 'ts').map((t) => t.text).join('')).to.equal(src);
  });

  it('classifies keywords, strings and numbers', () => {
    const tokens = highlight('const n = 42', 'ts');
    expect(tokens.find((t) => t.text === 'const')?.cls).to.equal('keyword');
    expect(tokens.find((t) => t.text === '42')?.cls).to.equal('number');
    expect(highlight('x = "s"', 'ts').find((t) => t.text === '"s"')?.cls).to.equal('string');
  });

  it('uses # comments for hash-comment languages', () => {
    expect(highlight('# a note', 'py').find((t) => t.cls === 'comment')?.text).to.equal('# a note');
    expect(highlight('# not a comment', 'ts').some((t) => t.cls === 'comment')).to.equal(false);
  });

  it('classifies literals apart from keywords', () => {
    expect(highlight('x = true', 'go').find((t) => t.text === 'true')?.cls).to.equal('literal');
  });
});

// ---------------------------------------------------------------------------
// diffs, paths and sizes
// ---------------------------------------------------------------------------

describe('splitDiffByFile', () => {
  const diff = [
    'diff --git a/one.txt b/one.txt',
    '@@ -1 +1 @@',
    '-a',
    '+b',
    'diff --git a/two.txt b/two.txt',
    '@@ -0,0 +1 @@',
    '+c',
  ].join('\n');

  it('splits on the diff --git markers', () => {
    const sections = splitDiffByFile(diff);
    expect(sections.map((s) => s.path)).to.deep.equal(['one.txt', 'two.txt']);
    expect(sections[1].patch).to.contain('+c');
    expect(sections[0].patch).to.not.contain('+c');
  });

  it('returns nothing for an empty diff', () => {
    expect(splitDiffByFile('')).to.deep.equal([]);
  });
});

describe('diffLineKind', () => {
  it('classifies each line of a unified diff', () => {
    expect(diffLineKind('@@ -1 +1 @@')).to.equal('hunk');
    expect(diffLineKind('diff --git a/x b/x')).to.equal('head');
    expect(diffLineKind('--- a/x')).to.equal('head');
    expect(diffLineKind('+added')).to.equal('add');
    expect(diffLineKind('-removed')).to.equal('del');
    expect(diffLineKind(' unchanged')).to.equal('context');
  });
});

describe('extOf', () => {
  it('takes the extension of a path', () => {
    expect(extOf('src/app/Main.TSX')).to.equal('tsx');
  });

  it('falls back to the bare filename when there is no extension', () => {
    expect(extOf('build/Makefile')).to.equal('makefile');
  });
});

describe('formatBytes', () => {
  it('renders — for an unmeasured size', () => {
    expect(formatBytes(0)).to.equal('—');
    expect(formatBytes(undefined)).to.equal('—');
  });

  it('scales into units', () => {
    expect(formatBytes(512)).to.equal('512 B');
    expect(formatBytes(2048)).to.equal('2.0 KB');
    expect(formatBytes(5 * 1024 * 1024)).to.equal('5.0 MB');
  });
});
