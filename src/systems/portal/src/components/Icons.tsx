/**
 * Nav glyphs — small line-style SVG icons for the sidebar's collapsed rail, drawn
 * in the same stroke idiom as {@link Logo}. They stroke with `currentColor`, so a
 * link's active/hover colour flows straight through (no per-icon theming needed).
 * Keys mirror the /app route segments; `settings` is the cog, etc.
 */

export type IconName =
  | 'workflows' | 'blueprints' | 'forge' | 'tickets' | 'events'
  | 'containers' | 'git' | 'outposts' | 'chaos' | 'argo'
  | 'builder' | 'audit' | 'gatekeeper' | 'settings' | 'projects';

/** Inner paths for each glyph, keyed by {@link IconName}; rendered inside a shared 24×24 stroke frame. */
const PATHS: Record<IconName, JSX.Element> = {
  // pipeline: two inputs merging into one downstream node
  workflows: (
    <>
      <circle cx="5" cy="6" r="2" />
      <circle cx="5" cy="18" r="2" />
      <circle cx="19" cy="12" r="2" />
      <path d="M7 6h6a4 4 0 0 1 4 4M7 18h6a4 4 0 0 0 4-4" />
    </>
  ),
  // stacked infrastructure layers (IaC state)
  blueprints: (
    <>
      <path d="M12 3l8 4-8 4-8-4 8-4z" />
      <path d="M4 12l8 4 8-4M4 16.5l8 4 8-4" />
    </>
  ),
  // flame
  forge: (
    <path d="M12 3c1 3 4 4 4 8a4 4 0 0 1-8 0c0-2 .8-3 2-4 .4 1.6 2 2 2 0z" />
  ),
  // ticket stub with a perforation
  tickets: (
    <>
      <path d="M4 8a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2 2 2 0 0 0 0 4 2 2 0 0 1-2 2H6a2 2 0 0 1-2-2 2 2 0 0 0 0-4z" />
      <path d="M14 6.5v11" strokeDasharray="2 2" />
    </>
  ),
  // radiating signal (event)
  events: (
    <path d="M15 4a3 3 0 0 0-3 3v7a3 3 0 1 1-3-3" />
  ),
  // 3D box
  containers: (
    <>
      <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3z" />
      <path d="M4 7.5l8 4.5 8-4.5M12 12v9" />
    </>
  ),
  // git branch
  git: (
    <>
      <circle cx="18" cy="6" r="2.5" />
      <circle cx="6" cy="18" r="2.5" />
      <path d="M6 3v12.5" />
      <path d="M18 8.5a9 9 0 0 1-9 9" />
    </>
  ),
  // broadcast / signal (outposts dial out)
  outposts: (
    <>
      <circle cx="12" cy="12" r="1.6" />
      <path d="M8 8a6 6 0 0 0 0 8M16 8a6 6 0 0 1 0 8M5 5a10 10 0 0 0 0 14M19 5a10 10 0 0 1 0 14" />
    </>
  ),
  // lightning bolt
  chaos: (
    <path d="M13 2 4 14h6l-1 8 9-12h-6l1-8z" />
  ),
  // gitops sync arrows
  argo: (
    <>
      <path d="M4 12a8 8 0 0 1 13.7-5.7L20 8.5M20 4v4.5h-4.5" />
      <path d="M20 12a8 8 0 0 1-13.7 5.7L4 15.5M4 20v-4.5h4.5" />
    </>
  ),
  // wrench
  builder: (
    <path d="M14.7 6.3a4 4 0 0 0-5.2 5.2L4 17l3 3 5.5-5.5a4 4 0 0 0 5.2-5.2l-2.7 2.7-2.5-2.5 2.7-2.7z" />
  ),
  // audit log — document with lines
  audit: (
    <>
      <rect x="5" y="4" width="14" height="17" rx="1" />
      <path d="M9 4V3h6v1M8.5 10h7M8.5 14h7M8.5 18h4" />
    </>
  ),
  // shield with keyhole
  gatekeeper: (
    <>
      <path d="M12 3l7 3v5c0 4-3 7-7 9-4-2-7-5-7-9V6l7-3z" />
      <circle cx="12" cy="11" r="1.4" />
      <path d="M12 12.4v2.4" />
    </>
  ),
  // folder tab (a project — a grouping scope)
  projects: (
    <>
      <path d="M3 7a1 1 0 0 1 1-1h5l2 2h8a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V7z" />
      <path d="M3 11h18" />
    </>
  ),
  // cog
  settings: (
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5 5l2.1 2.1M16.9 16.9 19 19M19 5l-2.1 2.1M7.1 16.9 5 19" />
    </>
  ),
};

// Nerd Font (Symbols) codepoints per icon — a subset of the font is bundled (nerdfont.css).
// Names not listed here fall back to the SVG glyph above, so nothing ever renders blank.
const NERD: Partial<Record<IconName, number>> = {
  workflows: 0xf07e5,  // md-pipe (pipeline)
  builder: 0xf06a9,    // md-robot (the coding agents)
  tickets: 0xf145,     // fa-ticket
  blueprints: 0xf02d,  // fa-book (wiki / source of truth)
  git: 0xe725,         // dev-git_branch (repos)
  argo: 0xf1e6,        // plug (git connector)
  forge: 0xf120,       // terminal
  events: 0xf0e7,      // bolt
  containers: 0xf1b2,  // cube
  outposts: 0xf233,    // server
  settings: 0xf013,    // cog
  gatekeeper: 0xf023,  // lock
  projects: 0xf07b,    // folder
};

/** Render a nav glyph by name: a Nerd Font glyph where mapped, else the line-style SVG. */
export function Icon({ name, size = 17 }: { name: IconName; size?: number }) {
  const cp = NERD[name];
  if (cp !== undefined) {
    return (
      <span className="nf" aria-hidden="true"
        style={{ fontSize: Math.round(size * 1.05), width: size + 3, flexShrink: 0 }}>
        {String.fromCodePoint(cp)}
      </span>
    );
  }
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none"
      stroke="currentColor" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round"
      aria-hidden="true" style={{ display: 'block', flexShrink: 0 }}>
      {PATHS[name]}
    </svg>
  );
}
