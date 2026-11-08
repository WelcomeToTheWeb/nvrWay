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
        /* the world's trouble fields — state lives in these fills, and the
           same fact must read identical on a tile back and a list badge */
        field: {
          red: 'var(--field-red)',
          'red-ink': 'var(--field-red-ink)',
          amber: 'var(--field-amber)',
          'amber-ink': 'var(--field-amber-ink)',
        },
      },
      // The world's voice is Archivo, self-hosted in public/fonts and loaded
      // from src/index.css: weight 300 carries display numerals and the
      // wordmark, 600/700 carry labels and data. `cap` keeps its name (the
      // pages use it for labels) but is no longer a narrow stamp face —
      // Metro states labels with weight, not uppercase tracking.
      fontFamily: {
        sans: ['Archivo', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'monospace'],
        cap: ['Archivo', 'ui-sans-serif', 'system-ui', 'sans-serif'],
      },
      /* Metro fields: zero radius everywhere. A tile is a rectangle. */
      borderRadius: {
        none: '0',
        sm: '0',
        DEFAULT: '0',
        md: '0',
        lg: '0',
        xl: '0',
        '2xl': '0',
        '3xl': '0',
        /* even `full` is zero: a Metro tile has no circle. The one shape
           this world draws is the rectangle; leaving 9999px here is the
           hole a future rounded-full would fall through. */
        full: '0',
      },
      letterSpacing: {
        cap: '0.01em',
        stamp: '0.02em',
      },
      transitionTimingFunction: {
        overshoot: 'cubic-bezier(0.34, 1.56, 0.64, 1)',
      },
    },
  },
  plugins: [],
};

export default config;
