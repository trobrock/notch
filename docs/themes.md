# Themes

Notch's fullscreen UI uses semantic themes. Set `theme` in the global config (`~/.notch/config.json`, or `$NOTCH_HOME/config.json`) or project config (`<cwd>/.notch/config.json`):

```json
{
  "theme": "rose-pine"
}
```

Project config overrides global config. An unknown configured name is a startup error that lists every available built-in and custom theme.

At runtime, `/theme` lists all loaded themes and `/theme NAME` applies one immediately. A runtime choice lasts only for the current process: it does not edit either config file.

## Built-ins

- `dark` (default): Pi's dark palette, with a charcoal user box, blue/teal accents, green/red tool status backgrounds, and blue-to-purple thinking levels.
- `dracula`: near-white text, cyan accents, purple-gray surfaces, green/yellow/pink/purple thinking levels, and Dracula red errors.
- `catppuccin-mocha`: Mocha text and surfaces, blue accents, teal/purple/pink thinking levels, and peach/red notices and errors.

## Custom JSON themes

Notch loads direct `.json` children from these directories, in order:

1. `$NOTCH_HOME/themes`, or `~/.notch/themes` when `NOTCH_HOME` is unset;
2. `<cwd>/.notch/themes`.

Later files replace earlier themes with the same normalized name, so a project theme overrides a user theme. A custom theme can also override a built-in. Names are case-insensitive; spaces and underscores normalize to hyphens. Additional directories can replace the defaults with `theme_dirs`:

```json
{
  "theme_dirs": ["/home/me/themes", "/work/project/.notch/themes"]
}
```

A minimal theme needs only the colors it changes. Missing roles inherit from `base`, which defaults to `dark`:

```json
{
  "name": "rose-pine",
  "base": "dark",
  "vars": {
    "text": "#e0def4",
    "muted": "#908caa",
    "surface": "#26233a",
    "foam": "#9ccfd8"
  },
  "colors": {
    "text": "text",
    "muted": "muted",
    "accent": "foam",
    "border": "muted",
    "userMessageBg": "surface",
    "userMessageText": "text",
    "mdHeading": "#f6c177",
    "mdCode": "#ebbcba",
    "toolSuccessBg": "#283b35",
    "toolErrorBg": "#3b2734",
    "error": "#eb6f92",
    "thinkingHigh": "#c4a7e7",
    "thinkingXhigh": "#eb6f92"
  }
}
```

`name` is optional and falls back to the filename; normalized names use only letters, numbers, and hyphens. `vars` is optional; a variable can refer to another variable. Every final value must be `#RRGGBB`. Notch converts colors to terminal SGR sequences itself, so theme files cannot inject terminal controls. Files larger than 1 MiB, malformed JSON, unknown roles, missing variables, invalid colors, and inheritance cycles are reported and skipped.

Supported foreground roles are:

```text
text, muted, accent, border
mdHeading, mdLink, mdLinkUrl, mdCode, mdCodeBlock
mdCodeBlockBorder, mdQuote, mdQuoteBorder, mdHr, mdListBullet
userMessageText, toolTitle, toolOutput
notice (or warning), error, composer, footer (or dim)
thinkingOff, thinkingMinimal, thinkingLow, thinkingMedium,
thinkingHigh, thinkingXhigh
```

Supported background roles are:

```text
userMessageBg, toolPendingBg, toolSuccessBg, toolErrorBg
```

The shape intentionally follows Pi theme files: `$schema`, `export`, and known Pi roles that Notch does not render are accepted and ignored. This allows many Pi palettes to load directly while keeping Notch's smaller semantic surface. Use `base` when a source theme leaves Notch-specific roles unspecified.

An example is available at [`examples/themes/rose-pine.json`](../examples/themes/rose-pine.json).

Themes color user and status-specific tool boxes, notices/errors, the footer, and editor borders for each thinking level. They also provide semantic transcript colors for Markdown headings, links and displayed URLs, inline and fenced code, blockquote/code bars, thematic rules, and list bullets. Bold, italic, and link underline use terminal attributes. User Markdown restores the user-card colors after each inline style, while assistant prose remains unboxed and no theme forces a page-wide terminal background.

See the [TUI layout](tui.md#pi-style-layout) and [thinking controls](tui.md#commands-and-thinking-level).
