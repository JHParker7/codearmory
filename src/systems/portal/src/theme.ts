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
  'amber', 'amberSoft', 'red', 'redSoft', 'blue',
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
  accent: string; amberTok: string; redTok: string; blue: string;
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
    blue: s.blue,
  };
}

const SPECS: Spec[] = [
  {
    name: 'cyber', label: 'cyber',
    bg: '#070907', bgAlt: '#0a0d0a', card: '#0c100c', cardHi: '#10150f',
    border: '#1a2018', borderHi: '#2a3328',
    text: '#d6dcd2', textHi: '#eef1eb', dim: '#7d8a78', faint: '#4a5346',
    accent: '78% 0.16 145', amberTok: '82% 0.14 80', redTok: '70% 0.20 22', blue: 'oklch(74% 0.14 230)',
  },
  {
    name: 'tokyo-night', label: 'tokyo night',
    bg: '#1a1b26', bgAlt: '#16161e', card: '#1f2335', cardHi: '#24283b',
    border: '#2a2e42', borderHi: '#3b4261',
    text: '#c0caf5', textHi: '#ffffff', dim: '#787c99', faint: '#565f89',
    accent: '75% 0.13 250', amberTok: '80% 0.12 70', redTok: '68% 0.19 18', blue: 'oklch(75% 0.13 250)',
  },
  {
    name: 'light-cyber', label: 'light cyber', light: true,
    bg: '#f5f7f3', bgAlt: '#ebeee8', card: '#ffffff', cardHi: '#f0f3ec',
    border: '#d2d9c9', borderHi: '#b9c2ad',
    text: '#1c241a', textHi: '#0c120a', dim: '#55604f', faint: '#8a937f',
    accent: '60% 0.17 145', amberTok: '66% 0.15 70', redTok: '56% 0.21 25', blue: 'oklch(56% 0.15 240)',
  },
  {
    name: 'dracula', label: 'dracula',
    bg: '#282a36', bgAlt: '#21222c', card: '#2a2c3a', cardHi: '#343746',
    border: '#3a3d4d', borderHi: '#4d5066',
    text: '#f8f8f2', textHi: '#ffffff', dim: '#7a82b8', faint: '#6272a4',
    accent: '76% 0.18 350', amberTok: '82% 0.14 80', redTok: '68% 0.19 18', blue: 'oklch(75% 0.13 260)',
  },
  {
    name: 'nord', label: 'nord',
    bg: '#2e3440', bgAlt: '#272c36', card: '#3b4252', cardHi: '#434c5e',
    border: '#434c5e', borderHi: '#4c566a',
    text: '#d8dee9', textHi: '#eceff4', dim: '#7b8494', faint: '#4c566a',
    accent: '80% 0.07 220', amberTok: '82% 0.10 80', redTok: '68% 0.14 25', blue: 'oklch(80% 0.07 220)',
  },
  {
    name: 'gruvbox', label: 'gruvbox',
    bg: '#282828', bgAlt: '#1d2021', card: '#32302f', cardHi: '#3c3836',
    border: '#3c3836', borderHi: '#504945',
    text: '#ebdbb2', textHi: '#fbf1c7', dim: '#a89984', faint: '#7c6f64',
    accent: '75% 0.15 130', amberTok: '80% 0.14 75', redTok: '62% 0.19 28', blue: 'oklch(70% 0.10 220)',
  },
  {
    name: 'catppuccin', label: 'catppuccin',
    bg: '#1e1e2e', bgAlt: '#181825', card: '#252537', cardHi: '#2a2a3c',
    border: '#313244', borderHi: '#45475a',
    text: '#cdd6f4', textHi: '#ffffff', dim: '#9399b2', faint: '#6c7086',
    accent: '76% 0.12 300', amberTok: '83% 0.10 80', redTok: '72% 0.16 15', blue: 'oklch(76% 0.10 240)',
  },
  {
    name: 'solarized', label: 'solarized',
    bg: '#002b36', bgAlt: '#00252e', card: '#073642', cardHi: '#0a3f4d',
    border: '#0e4b59', borderHi: '#586e75',
    text: '#93a1a1', textHi: '#fdf6e3', dim: '#839496', faint: '#586e75',
    accent: '68% 0.15 130', amberTok: '70% 0.13 70', redTok: '60% 0.17 30', blue: 'oklch(62% 0.11 240)',
  },
];

export interface ThemeMeta { name: string; label: string; accent: string; bg: string; light: boolean; }

export const THEMES: ThemeMeta[] = SPECS.map(s => ({
  name: s.name, label: s.label, accent: `oklch(${s.accent})`, bg: s.bg, light: !!s.light,
}));

const PALETTES: Record<string, Palette> = Object.fromEntries(SPECS.map(s => [s.name, palette(s)]));

export const DEFAULT_THEME = 'cyber';
const STORAGE_KEY = 'ca-theme';

/** Return the persisted theme name if valid, else the default. Tolerates localStorage being unavailable. */
export function getStoredTheme(): string {
  try {
    const t = localStorage.getItem(STORAGE_KEY);
    if (t && PALETTES[t]) return t;
  } catch { /* ignore */ }
  return DEFAULT_THEME;
}

/** Write the named palette's tokens onto :root as `--ca-*` CSS vars and persist the choice (falling back to the default for an unknown name). */
export function applyTheme(name: string): void {
  const pal = PALETTES[name] ?? PALETTES[DEFAULT_THEME];
  const root = document.documentElement;
  for (const key of KEYS) root.style.setProperty(`--ca-${key}`, pal[key]);
  try { localStorage.setItem(STORAGE_KEY, PALETTES[name] ? name : DEFAULT_THEME); } catch { /* ignore */ }
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
  mono: MONO,
} as const;
