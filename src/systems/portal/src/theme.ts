/**
 * Theme system — palettes are applied as CSS custom properties on :root so the
 * whole app re-themes live. `T` references those vars, so components keep using
 * `T.green`, `T.bg`, … unchanged. The eight palettes mirror the armory TUI.
 */

const MONO = '"JetBrains Mono", ui-monospace, "SF Mono", Menlo, monospace';

// The styling tokens every component consumes (everything except the static font).
const KEYS = [
  'bg', 'bgAlt', 'card', 'cardHi', 'border', 'borderHi',
  'text', 'textHi', 'dim', 'faint',
  'green', 'greenSoft', 'greenFaint',
  'amber', 'amberSoft', 'red', 'redSoft', 'blue', 'blueSoft',
] as const;

type Key = (typeof KEYS)[number];
export type Palette = Record<Key, string>;

// A theme is specified compactly; soft/faint accent variants are derived so the
// accent color stays internally consistent. `accent`/`amberTok`/`redTok` are the
// interior of an oklch(...) expression so we can append an alpha for soft tints.
interface Spec {
  name: string;
  label: string;
  light?: boolean;
  bg: string; bgAlt: string; card: string; cardHi: string;
  border: string; borderHi: string;
  text: string; textHi: string; dim: string; faint: string;
  accent: string; amberTok: string; redTok: string; blueTok: string;
}

/** Expand a compact {@link Spec} into a full {@link Palette}, deriving the soft/faint accent tints from the accent oklch. */
function palette(s: Spec): Palette {
  return {
    bg: s.bg, bgAlt: s.bgAlt, card: s.card, cardHi: s.cardHi,
    border: s.border, borderHi: s.borderHi,
    text: s.text, textHi: s.textHi, dim: s.dim, faint: s.faint,
    green: `oklch(${s.accent})`,
    greenSoft: `oklch(${s.accent} / 0.12)`,
    greenFaint: `oklch(${s.accent} / 0.04)`,
    amber: `oklch(${s.amberTok})`,
    amberSoft: `oklch(${s.amberTok} / 0.12)`,
    red: `oklch(${s.redTok})`,
    redSoft: `oklch(${s.redTok} / 0.12)`,
    blue: `oklch(${s.blueTok})`,
    blueSoft: `oklch(${s.blueTok} / 0.12)`,
  };
}

const SPECS: Spec[] = [
  {
    name: 'cyber', label: 'cyber',
    bg: '#070907', bgAlt: '#0a0d0a', card: '#0c100c', cardHi: '#10150f',
    border: '#1a2018', borderHi: '#2a3328',
    text: '#d6dcd2', textHi: '#eef1eb', dim: '#7d8a78', faint: '#4a5346',
    accent: '78% 0.16 145', amberTok: '82% 0.14 80', redTok: '70% 0.20 22', blueTok: '74% 0.14 230',
  },
  {
    name: 'tokyo-night', label: 'tokyo night',
    bg: '#1a1b26', bgAlt: '#16161e', card: '#1f2335', cardHi: '#24283b',
    border: '#2a2e42', borderHi: '#3b4261',
    text: '#c0caf5', textHi: '#ffffff', dim: '#787c99', faint: '#565f89',
    accent: '75% 0.13 250', amberTok: '80% 0.12 70', redTok: '68% 0.19 18', blueTok: '75% 0.13 250',
  },
  {
    name: 'light-cyber', label: 'light cyber', light: true,
    bg: '#f5f7f3', bgAlt: '#ebeee8', card: '#ffffff', cardHi: '#f0f3ec',
    border: '#d2d9c9', borderHi: '#b9c2ad',
    text: '#1c241a', textHi: '#0c120a', dim: '#55604f', faint: '#8a937f',
    accent: '60% 0.17 145', amberTok: '66% 0.15 70', redTok: '56% 0.21 25', blueTok: '56% 0.15 240',
  },
  {
    name: 'dracula', label: 'dracula',
    bg: '#282a36', bgAlt: '#21222c', card: '#2a2c3a', cardHi: '#343746',
    border: '#3a3d4d', borderHi: '#4d5066',
    text: '#f8f8f2', textHi: '#ffffff', dim: '#7a82b8', faint: '#6272a4',
    accent: '76% 0.18 350', amberTok: '82% 0.14 80', redTok: '68% 0.19 18', blueTok: '75% 0.13 260',
  },
  {
    name: 'nord', label: 'nord',
    bg: '#2e3440', bgAlt: '#272c36', card: '#3b4252', cardHi: '#434c5e',
    border: '#434c5e', borderHi: '#4c566a',
    text: '#d8dee9', textHi: '#eceff4', dim: '#7b8494', faint: '#4c566a',
    accent: '80% 0.07 220', amberTok: '82% 0.10 80', redTok: '68% 0.14 25', blueTok: '80% 0.07 220',
  },
  {
    name: 'gruvbox', label: 'gruvbox',
    bg: '#282828', bgAlt: '#1d2021', card: '#32302f', cardHi: '#3c3836',
    border: '#3c3836', borderHi: '#504945',
    text: '#ebdbb2', textHi: '#fbf1c7', dim: '#a89984', faint: '#7c6f64',
    accent: '75% 0.15 130', amberTok: '80% 0.14 75', redTok: '62% 0.19 28', blueTok: '70% 0.10 220',
  },
  {
    name: 'catppuccin', label: 'catppuccin',
    bg: '#1e1e2e', bgAlt: '#181825', card: '#252537', cardHi: '#2a2a3c',
    border: '#313244', borderHi: '#45475a',
    text: '#cdd6f4', textHi: '#ffffff', dim: '#9399b2', faint: '#6c7086',
    accent: '76% 0.12 300', amberTok: '83% 0.10 80', redTok: '72% 0.16 15', blueTok: '76% 0.10 240',
  },
  {
    name: 'solarized', label: 'solarized',
    bg: '#002b36', bgAlt: '#00252e', card: '#073642', cardHi: '#0a3f4d',
    border: '#0e4b59', borderHi: '#586e75',
    text: '#93a1a1', textHi: '#fdf6e3', dim: '#839496', faint: '#586e75',
    accent: '68% 0.15 130', amberTok: '70% 0.13 70', redTok: '60% 0.17 30', blueTok: '62% 0.11 240',
  },
];

export interface ThemeMeta { name: string; label: string; accent: string; bg: string; light: boolean; }

export const THEMES: ThemeMeta[] = SPECS.map(s => ({
  name: s.name, label: s.label, accent: `oklch(${s.accent})`, bg: s.bg, light: !!s.light,
}));

const PALETTES: Record<string, Palette> = Object.fromEntries(SPECS.map(s => [s.name, palette(s)]));

// The raw accent token per theme, kept alongside PALETTES because the favicon needs the
// oklch *numbers* rather than the `oklch(...)` string the palette stores.
const ACCENTS: Record<string, string> = Object.fromEntries(SPECS.map(s => [s.name, s.accent]));

export const DEFAULT_THEME = 'cyber';
const STORAGE_KEY = 'ca-theme';

/**
 * Convert an accent token (`'78% 0.16 145'` — the interior of an oklch() expression, as
 * {@link Spec.accent} stores it) to an `#rrggbb` sRGB hex, clamped to gamut.
 *
 * The favicon is an SVG data URI, which resolves no CSS vars and inherits no page styles,
 * so the accent has to be a literal colour by the time it goes in. Hex rather than a raw
 * `oklch()` string because favicon rendering is the one place where a colour the browser
 * fails to parse degrades to *no icon at all*.
 */
export function oklchToHex(token: string): string {
  const [lRaw, cRaw, hRaw] = token.trim().split(/\s+/);
  const L = parseFloat(lRaw) / (lRaw.endsWith('%') ? 100 : 1);
  const C = parseFloat(cRaw);
  const h = (parseFloat(hRaw) * Math.PI) / 180;
  const a = C * Math.cos(h);
  const b = C * Math.sin(h);

  // OKLab → LMS → (cubed) → linear sRGB, using Björn Ottosson's reference matrices.
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.2914855480 * b) ** 3;

  const linear = [
    4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
    -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
    -0.0041960863 * l - 0.7034186147 * m + 1.7076147010 * s,
  ];
  const channel = (v: number) => {
    const c = Math.min(1, Math.max(0, v));
    const encoded = c <= 0.0031308 ? 12.92 * c : 1.055 * c ** (1 / 2.4) - 0.055;
    return Math.round(encoded * 255).toString(16).padStart(2, '0');
  };
  return `#${linear.map(channel).join('')}`;
}

/**
 * Repaint the browser-tab icon in `color`. The glyph is the mark from
 * components/Logo.tsx — keep the two in step. index.html carries a static copy in the
 * default accent so the tab is already right before this module loads; this replaces it
 * with the live theme's. The old link is removed rather than mutated because browsers
 * are unreliable about re-reading a favicon whose href changed in place.
 */
function setFavicon(color: string): void {
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 22 22" fill="none" stroke="${color}">` +
    '<rect x="2" y="2" width="18" height="18" stroke-width="1.4"/>' +
    '<path d="M6 7l4 4-4 4M11 15h5" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>' +
    '</svg>';
  document.querySelectorAll('link[rel="icon"]').forEach(el => el.remove());
  const link = document.createElement('link');
  link.rel = 'icon';
  link.type = 'image/svg+xml';
  link.href = `data:image/svg+xml,${encodeURIComponent(svg)}`;
  document.head.appendChild(link);
}

/** Return the persisted theme name if valid, else the default. Tolerates localStorage being unavailable. */
export function getStoredTheme(): string {
  try {
    const t = localStorage.getItem(STORAGE_KEY);
    if (t && PALETTES[t]) return t;
  } catch { /* ignore */ }
  return DEFAULT_THEME;
}

/** Write the named palette's tokens onto :root as `--ca-*` CSS vars, retint the favicon, and persist the choice (falling back to the default for an unknown name). */
export function applyTheme(name: string): void {
  const resolved = PALETTES[name] ? name : DEFAULT_THEME;
  const root = document.documentElement;
  for (const key of KEYS) root.style.setProperty(`--ca-${key}`, PALETTES[resolved][key]);
  setFavicon(oklchToHex(ACCENTS[resolved]));
  try { localStorage.setItem(STORAGE_KEY, resolved); } catch { /* ignore */ }
}

/** Styling token map components consume; each value resolves to a live CSS var, so applyTheme() must run once before first paint. */
export const T = {
  bg: 'var(--ca-bg)',
  bgAlt: 'var(--ca-bgAlt)',
  card: 'var(--ca-card)',
  cardHi: 'var(--ca-cardHi)',
  border: 'var(--ca-border)',
  borderHi: 'var(--ca-borderHi)',
  text: 'var(--ca-text)',
  textHi: 'var(--ca-textHi)',
  dim: 'var(--ca-dim)',
  faint: 'var(--ca-faint)',
  green: 'var(--ca-green)',
  greenSoft: 'var(--ca-greenSoft)',
  greenFaint: 'var(--ca-greenFaint)',
  amber: 'var(--ca-amber)',
  amberSoft: 'var(--ca-amberSoft)',
  red: 'var(--ca-red)',
  redSoft: 'var(--ca-redSoft)',
  blue: 'var(--ca-blue)',
  blueSoft: 'var(--ca-blueSoft)',
  mono: MONO,
} as const;
