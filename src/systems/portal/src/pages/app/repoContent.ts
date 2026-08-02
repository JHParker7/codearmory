/**
 * Pure content helpers for the repos/ page: a small Markdown parser, a
 * dependency-free syntax highlighter, and unified-diff splitting/classification.
 *
 * Everything here returns DATA (token trees), never markup. README and file
 * contents are untrusted — anyone who can push can write them — so nothing on
 * this path can produce HTML for the page to inject; React renders the tokens as
 * text nodes and the unsafe failure mode simply does not exist. That is also why
 * links are filtered here rather than at render time: a `javascript:`/`data:` URL
 * loses its href and stays plain text, and so does a relative one (it cannot be
 * resolved to anything meaningful outside the repo).
 */

// ── inline markdown ──────────────────────────────────────────────────────────

export type MdInline =
  | { kind: 'text'; text: string }
  | { kind: 'code'; text: string }
  | { kind: 'strong'; children: MdInline[] }
  | { kind: 'em'; children: MdInline[] }
  | { kind: 'del'; children: MdInline[] }
  | { kind: 'link'; href: string; children: MdInline[] };

export type MdBlock =
  | { kind: 'heading'; level: number; children: MdInline[] }
  | { kind: 'paragraph'; children: MdInline[] }
  | { kind: 'list'; ordered: boolean; items: MdInline[][] }
  | { kind: 'quote'; children: MdInline[] }
  | { kind: 'code'; lang: string; text: string }
  | { kind: 'rule' }
  | { kind: 'table'; head: MdInline[][]; rows: MdInline[][][] };

/** Only http(s) links keep an href; anything else renders as its own text. */
function isSafeHref(url: string): boolean {
  return /^https?:\/\//i.test(url);
}

// Sticky (anchored) patterns, tried in order at each position. Anchoring rather
// than scanning means the underscore forms can check their word boundaries
// against the surrounding characters directly — no lookbehind, which is still
// not universally available in the browsers this ships to.
const CODE_RE = /`([^`]+)`/y;
const IMAGE_RE = /!\[([^\]]*)\]\(([^)\s]+)[^)]*\)/y;
const LINK_RE = /\[([^\]]+)\]\(([^)\s]+)[^)]*\)/y;
const STRONG_STAR_RE = /\*\*(?=\S)([\s\S]*?\S)\*\*/y;
const STRONG_UNDER_RE = /__(?=\S)([\s\S]*?\S)__/y;
const EM_STAR_RE = /\*(?=\S)([^*]*?\S)\*/y;
const EM_UNDER_RE = /_(?=\S)([^_]*?\S)_/y;
const DEL_RE = /~~(?=\S)([\s\S]*?\S)~~/y;

const isWordChar = (c: string | undefined) => !!c && /\w/.test(c);

/**
 * Parse inline markdown into a token tree. Asterisks emphasise anywhere;
 * underscores only at word boundaries — GitHub-flavoured behaviour, so an
 * identifier like codearmory_git_factory keeps its underscores instead of
 * rendering as codearmory<em>git</em>factory.
 */
export function parseInline(src: string): MdInline[] {
  const out: MdInline[] = [];
  let text = '';
  const flush = () => { if (text) { out.push({ kind: 'text', text }); text = ''; } };

  let i = 0;
  while (i < src.length) {
    const at = (re: RegExp): RegExpExecArray | null => { re.lastIndex = i; return re.exec(src); };
    const prev = i > 0 ? src[i - 1] : undefined;
    const c = src[i];

    let m: RegExpExecArray | null = null;
    if (c === '`' && (m = at(CODE_RE))) {
      flush();
      out.push({ kind: 'code', text: m[1] });
      i = CODE_RE.lastIndex;
      continue;
    }
    // Images become their alt text: no remote loads from an untrusted readme.
    if (c === '!' && (m = at(IMAGE_RE))) {
      flush();
      text += m[1];
      i = IMAGE_RE.lastIndex;
      continue;
    }
    if (c === '[' && (m = at(LINK_RE))) {
      flush();
      const children = parseInline(m[1]);
      if (isSafeHref(m[2])) out.push({ kind: 'link', href: m[2], children });
      else out.push(...children);
      i = LINK_RE.lastIndex;
      continue;
    }
    if (c === '*' && (m = at(STRONG_STAR_RE))) {
      flush();
      out.push({ kind: 'strong', children: parseInline(m[1]) });
      i = STRONG_STAR_RE.lastIndex;
      continue;
    }
    if (c === '_' && !isWordChar(prev) && (m = at(STRONG_UNDER_RE)) && !isWordChar(src[STRONG_UNDER_RE.lastIndex])) {
      flush();
      out.push({ kind: 'strong', children: parseInline(m[1]) });
      i = STRONG_UNDER_RE.lastIndex;
      continue;
    }
    if (c === '*' && (m = at(EM_STAR_RE))) {
      flush();
      out.push({ kind: 'em', children: parseInline(m[1]) });
      i = EM_STAR_RE.lastIndex;
      continue;
    }
    if (c === '_' && !isWordChar(prev) && (m = at(EM_UNDER_RE)) && !isWordChar(src[EM_UNDER_RE.lastIndex])) {
      flush();
      out.push({ kind: 'em', children: parseInline(m[1]) });
      i = EM_UNDER_RE.lastIndex;
      continue;
    }
    if (c === '~' && (m = at(DEL_RE))) {
      flush();
      out.push({ kind: 'del', children: parseInline(m[1]) });
      i = DEL_RE.lastIndex;
      continue;
    }

    text += c;
    i++;
  }
  flush();
  return out;
}

// ── block markdown ───────────────────────────────────────────────────────────

const cells = (row: string) => row.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());

/**
 * Parse a deliberately small Markdown subset — headings, emphasis, code (inline
 * and fenced), links, lists, quotes, rules and pipe tables. Anything else is
 * carried through as paragraph text, which is the readable failure mode.
 */
export function parseMarkdown(src: string): MdBlock[] {
  const lines = (src ?? '').replace(/\r\n/g, '\n').split('\n');
  const out: MdBlock[] = [];

  let para: string[] = [];
  let quote: string[] = [];
  let list: { ordered: boolean; items: string[] } | null = null;

  const flushPara = () => { if (para.length) { out.push({ kind: 'paragraph', children: parseInline(para.join(' ')) }); para = []; } };
  const flushQuote = () => { if (quote.length) { out.push({ kind: 'quote', children: parseInline(quote.join(' ')) }); quote = []; } };
  const flushList = () => {
    if (list) { out.push({ kind: 'list', ordered: list.ordered, items: list.items.map(parseInline) }); list = null; }
  };
  const flushAll = () => { flushPara(); flushList(); flushQuote(); };

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    // Fenced code: consumed whole, so no inline rule can touch its contents.
    const fence = line.match(/^\s*```([\w+-]*)\s*$/);
    if (fence) {
      flushAll();
      const body: string[] = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) { body.push(lines[i]); i++; }
      out.push({ kind: 'code', lang: fence[1] ?? '', text: body.join('\n') });
      continue;
    }

    if (!line.trim()) { flushAll(); continue; }

    let m: RegExpMatchArray | null;
    if ((m = line.match(/^(#{1,4})\s+(.*)$/))) {
      flushAll();
      out.push({ kind: 'heading', level: m[1].length, children: parseInline(m[2]) });
      continue;
    }
    if (/^\s*([-*_])\s*\1\s*\1[-*_\s]*$/.test(line)) {
      flushAll();
      out.push({ kind: 'rule' });
      continue;
    }
    if ((m = line.match(/^>\s?(.*)$/))) {
      flushPara(); flushList();
      quote.push(m[1]);
      continue;
    }

    // Pipe table: a header row followed by a |---|---| separator.
    if (line.includes('|') && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[i + 1] ?? '')) {
      flushAll();
      const head = cells(line).map(parseInline);
      const rows: MdInline[][][] = [];
      i += 2;
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) {
        rows.push(cells(lines[i]).map(parseInline));
        i++;
      }
      i--;
      out.push({ kind: 'table', head, rows });
      continue;
    }

    if ((m = line.match(/^\s*[-*+]\s+(.*)$/))) {
      flushPara(); flushQuote();
      if (!list || list.ordered) { flushList(); list = { ordered: false, items: [] }; }
      list.items.push(m[1]);
      continue;
    }
    if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) {
      flushPara(); flushQuote();
      if (!list || !list.ordered) { flushList(); list = { ordered: true, items: [] }; }
      list.items.push(m[1]);
      continue;
    }

    flushList(); flushQuote();
    para.push(line.trim());
  }
  flushAll();
  return out;
}

// ── syntax highlighting ──────────────────────────────────────────────────────

/** Token class: comment, string, number, keyword, literal, or plain (''). */
export type TokenClass = '' | 'comment' | 'string' | 'number' | 'keyword' | 'literal';

export interface CodeToken { text: string; cls: TokenClass }

// Not a per-language grammar: one pass colours comments, strings, numbers and a
// broad keyword/literal set — enough to make code readable across most languages
// without shipping a highlighting library.
const HL_KEYWORDS = new Set((
  'abstract as async await break case catch chan class const continue def ' +
  'defer del delete do elif else end enum export extends fallthrough final finally fn for from func ' +
  'function go goto if impl implements import in include inline interface is lambda let loop map match ' +
  'mod module move mut namespace new not or override package private protected pub public raise range ' +
  'readonly record ref require return select self sizeof static struct super switch template then this ' +
  'throw trait try type typedef typeof union unless until use using var virtual void volatile when ' +
  'where while with yield and').split(' '));
const HL_LITERALS = new Set(['true', 'false', 'null', 'nil', 'none', 'undefined', 'True', 'False', 'None', 'NULL', 'NaN']);
const HL_HASH = new Set(['py', 'rb', 'sh', 'bash', 'zsh', 'yml', 'yaml', 'toml', 'ini', 'cfg', 'conf', 'pl', 'r', 'ex', 'exs', 'tf', 'dockerfile', 'makefile', 'mk', 'gitignore', 'env']);
const HL_DASH = new Set(['sql', 'lua', 'hs', 'elm', 'adb', 'ads', 'vhd', 'vhdl']);
const HL_BLOCK = new Set(['js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'go', 'c', 'cc', 'cpp', 'cxx', 'h', 'hpp', 'java', 'rs', 'cs', 'css', 'scss', 'less', 'php', 'swift', 'kt', 'kts', 'scala', 'proto', 'json5', 'glsl', 'dart', 'm', 'mm']);

/** Lowercase file extension (or the bare filename, for Makefile/Dockerfile-likes). */
export function extOf(path: string): string {
  const fn = (path || '').split('/').pop() ?? '';
  return (fn.includes('.') ? fn.split('.').pop()! : fn).toLowerCase();
}

/** Tokenize `src` for display. Adjacent plain text is merged, so the caller renders one span per coloured run. */
export function highlight(src: string, ext: string): CodeToken[] {
  const e = (ext || '').toLowerCase();
  const lineComment = HL_HASH.has(e) ? '#' : HL_DASH.has(e) ? '--' : '//';
  const lc = lineComment.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  // Each spec is [class, pattern]; every pattern contributes exactly one capture
  // group (no inner captures) so a matched group index maps back to its class.
  const specs: [TokenClass | 'word', string][] = [];
  if (HL_BLOCK.has(e)) specs.push(['comment', '/\\*[\\s\\S]*?\\*/']);
  specs.push(['comment', lc + '[^\\n]*']);
  specs.push(['string', '"(?:\\\\.|[^"\\\\\\n])*"|\'(?:\\\\.|[^\'\\\\\\n])*\'|`(?:\\\\.|[^`\\\\])*`']);
  specs.push(['number', '\\b0[xXbBoO][0-9a-fA-F_]+\\b|\\b\\d[\\d_]*(?:\\.\\d+)?(?:[eE][+-]?\\d+)?\\b']);
  specs.push(['word', '[A-Za-z_$][\\w$]*']);

  const re = new RegExp(specs.map((s) => `(${s[1]})`).join('|'), 'g');
  const out: CodeToken[] = [];
  const push = (text: string, cls: TokenClass) => {
    if (!text) return;
    const last = out[out.length - 1];
    if (cls === '' && last && last.cls === '') last.text += text;
    else out.push({ text, cls });
  };

  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(src))) {
    push(src.slice(last, m.index), '');
    last = re.lastIndex;
    let cls: TokenClass | 'word' = '';
    for (let i = 0; i < specs.length; i++) {
      if (m[i + 1] !== undefined) { cls = specs[i][0]; break; }
    }
    if (cls === 'word') cls = HL_LITERALS.has(m[0]) ? 'literal' : HL_KEYWORDS.has(m[0]) ? 'keyword' : '';
    push(m[0], cls);
    if (re.lastIndex === m.index) re.lastIndex++; // guard against a zero-width match
  }
  push(src.slice(last), '');
  return out;
}

// ── unified diffs ────────────────────────────────────────────────────────────

export interface DiffSection { path: string; patch: string }

/**
 * Carve a unified diff into one section per file (split on the `diff --git`
 * markers) so the UI can show a single file at a time instead of the whole thing.
 */
export function splitDiffByFile(diff: string): DiffSection[] {
  const out: { path: string; lines: string[] }[] = [];
  let cur: { path: string; lines: string[] } | null = null;
  for (const l of (diff || '').split('\n')) {
    if (l.startsWith('diff --git ')) {
      if (cur) out.push(cur);
      const m = l.match(/ b\/(.+)$/);
      cur = { path: m ? m[1] : l.slice(11), lines: [l] };
    } else if (cur) {
      cur.lines.push(l);
    }
  }
  if (cur) out.push(cur);
  return out.map((s) => ({ path: s.path, patch: s.lines.join('\n') }));
}

export type DiffLineKind = 'add' | 'del' | 'hunk' | 'head' | 'context';

/** Classify one line of a unified diff, for tinting it. */
export function diffLineKind(line: string): DiffLineKind {
  if (line.startsWith('@@')) return 'hunk';
  if (/^(diff --git|index |--- |\+\+\+ |new file|deleted file|rename |similarity )/.test(line)) return 'head';
  if (line.startsWith('+')) return 'add';
  if (line.startsWith('-')) return 'del';
  return 'context';
}

// ── misc formatting ──────────────────────────────────────────────────────────

/**
 * Repo/blob size as reported by the API (measured at the last push or repack).
 * 0 means never measured rather than empty, so it reads as "—" instead of a
 * confident "0 B".
 */
export function formatBytes(n: number | null | undefined): string {
  if (!n || n < 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}
