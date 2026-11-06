import type { Config } from 'tailwindcss';

// The token bridge. Components never write a hex colour: they write `bg-bg-card`
// or `text-status-offline`, and the value comes from a CSS custom property
// defined in src/index.css. That is what lets the console answer a question
// about the machine in one glance — `status-offline` is a fact about a camera,
// stated in the same words everywhere it appears — and it is what lets the
// zone editor paint with `--accent` at 20 % without a component knowing the
// hue.
//
// The shape is OurWay's: letterpress corners (rules meet at right angles,
// never balloons), a cap letterspacing for headings, and colour roles named
// after what they mean rather than what they look like. The roles themselves
// are nvrWay's: a camera's state, an event's severity, and the accent that
// marks the operator's own drawing.
const config: Config = {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        bg: {
          DEFAULT: 'var(--bg-primary)',
          secondary: 'var(--bg-secondary)',
          card: 'var(--bg-card)',
          border: 'var(--bg-border)',
        },
        text: {
          primary: 'var(--text-primary)',
          secondary: 'var(--text-secondary)',
          muted: 'var(--text-muted)',
        },
        /* the operator's own mark: zones drawn, vertices dragged, the live row */
        accent: {
          DEFAULT: 'var(--accent)',
          light: 'var(--accent-light)',
          dark: 'var(--accent-dark)',
        },
        /* a camera's truth, from camera.status — never a spinner */
        status: {
          online: 'var(--online)',
          offline: 'var(--offline)',
          warning: 'var(--warning)',
          error: 'var(--error)',
        },
        /* an event's severity, 1..5: noted, noteworthy, call somebody,
           intrusion, intrusion in the home */
        severity: {
          1: 'var(--severity-1)',
          2: 'var(--severity-2)',
          3: 'var(--severity-3)',
          4: 'var(--severity-4)',
          5: 'var(--severity-5)',
        },
        rule: {
          DEFAULT: 'var(--rule)',
          strong: 'var(--rule-strong)',
        },
      },
      // No faces are vendored yet: the stack names the intent and falls back
      // to the system UI font. Vendoring self-hosted woff2 files later is a
      // change to this block and a fonts.css, never to a component.
      fontFamily: {
        sans: ['Barlow', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'monospace'],
        cap: ['"Archivo Narrow"', 'Barlow', 'ui-sans-serif', 'sans-serif'],
      },
      /* Letterpress corners: rules meet at right angles, never balloons. */
      borderRadius: {
        none: '0',
        sm: '2px',
        DEFAULT: '2px',
        md: '2px',
        lg: '3px',
        xl: '3px',
        '2xl': '4px',
        '3xl': '6px',
        full: '9999px',
      },
      letterSpacing: {
        cap: '0.09em',
        stamp: '0.14em',
      },
      transitionTimingFunction: {
        overshoot: 'cubic-bezier(0.34, 1.56, 0.64, 1)',
      },
    },
  },
  plugins: [],
};

export default config;
