# Slide decks

Marp-flavoured markdown is the source. Each `.md` here is also readable
straight on GitHub — pagination markers (`---`) render as horizontal
rules in plain markdown, so the content is browseable as a single long
doc OR converted to slides.

## Available decks

| File                            | Audience                              |
|---------------------------------|---------------------------------------|
| `dpubnkctl-overview.md`         | Customer / new-hire intro (14 slides) |

## Content-density rule (avoid overflow)

Marp does **not** auto-detect overflow — content past ~720px just
clips silently in some browsers and renders past the slide bounds in
others. A 16:9 slide at the deck's default 23px body font fits roughly:

- **8 short bullets** OR
- **6 bullets + a 5-row table** OR
- **a single 10-line code block + 2 bullets**

If you need more, split into two slides — don't shrink the body font.
A `## H2` heading + section divider + tagline together consume ~4
visual lines before your first bullet renders. Tables, fenced code
blocks, and inline `![images]` all count toward the budget; nested
bullets count double.

Before committing slide edits: open the generated HTML in a browser
(`make slides-html` then file:///…/docs/slides/dpubnkctl-overview.html)
and step through every slide you touched — confirm nothing flows past
the bottom edge.

## Build artefacts

From the repo root:

```bash
make slides           # all three formats (HTML always; PPTX/PDF if browser available)
make slides-html      # HTML — works browser-free, opens locally or hosted as Pages
make slides-pptx      # PPTX — edit in PowerPoint / LibreOffice / Keynote
make slides-pdf       # PDF  — printable, attach to email
```

PPTX and PDF rely on Marp CLI driving a headless browser. If neither
Chromium nor Firefox is on PATH, set `CHROME_PATH=/path/to/chromium`
before running, or install one:

```bash
sudo apt install chromium-browser    # or firefox
```

Marp CLI itself is fetched on-demand via `npx`; no global npm install
needed. The build will download it the first time and cache under
`~/.npm/_npx/`.

## Editing for a customer presentation

Branch the markdown, tweak content, regenerate:

```bash
git checkout -b customer-deck-acme
$EDITOR docs/slides/dpubnkctl-overview.md
make slides
```

The HTML is self-contained; serve directly or paste into an email.
PPTX is friendly to last-mile slide edits (slide order, layout
tweaks, logos) before a real customer meeting.

## Web preview

GitHub renders the markdown as a doc. For an in-browser slide preview
without leaving the repo, push to a `gh-pages` branch (or enable Pages
on `main` / `docs/`) and link to the generated HTML, e.g.
`https://<user>.github.io/dpubnkctl/slides/dpubnkctl-overview.html`.
