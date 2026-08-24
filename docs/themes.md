# Themes

Notch's fullscreen UI uses Pi-style semantic themes. Set `theme` in the global config (`~/.notch/config.json`, or `$NOTCH_HOME/config.json`) or project config (`<cwd>/.notch/config.json`):

```json
{
  "theme": "catppuccin-mocha"
}
```

Project config overrides global config. An unknown configured name is a startup error that lists the available themes.

At runtime, `/theme` lists the built-ins and `/theme NAME` applies one immediately. A runtime choice lasts only for the current process: it does not edit either config file.

## Built-ins

- `dark` (default): Pi's dark palette, with a charcoal user box, blue/teal accents, green/red tool status backgrounds, and blue-to-purple thinking levels.
- `dracula`: near-white text, cyan accents, purple-gray surfaces, green/yellow/pink/purple thinking levels, and Dracula red errors.
- `catppuccin-mocha`: Mocha text and surfaces, blue accents, teal/purple/pink thinking levels, and peach/red notices and errors.

Themes color user and status-specific tool boxes, notices/errors, the footer, and the editor borders for each thinking level. They also provide semantic transcript colors for Markdown headings, links and displayed URLs, inline and fenced code, blockquote/code bars, thematic rules, and list bullets. Bold, italic, and link underline use terminal attributes. User Markdown restores the user-card colors after each inline style, while assistant prose remains unboxed and no theme forces a page-wide terminal background.

Only these built-ins are supported. Notch does not currently discover theme files or accept custom theme JSON. See the [TUI layout](tui.md#pi-style-layout) and [thinking controls](tui.md#commands-and-thinking-level).
