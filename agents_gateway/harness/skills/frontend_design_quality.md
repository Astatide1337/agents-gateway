# Frontend design quality — mandatory for any UI-facing task

Adapted from the "Taste Skill" anti-slop framework (github.com/Leonxlnx/taste-skill),
condensed for product/app UIs rather than landing pages. This is not optional
styling advice — treat every rule below as a hard requirement, same as a
functional spec. A UI that violates these looks like an unfinished student
project, not a product, and will be rejected in review.

## Stack — no exceptions unless the task explicitly says otherwise

- Framework: **Next.js** (App Router) with React. Not vanilla JS/HTML/CSS
  unless a real constraint (documented in the task, not assumed) forces it.
- Styling: **Tailwind CSS**.
- Components: **shadcn/ui** (`npx shadcn@latest add ...`) as the default
  component foundation — never ship a shadcn component in its unmodified
  default state; adjust radii, spacing, and color to fit the product.
  Radix UI primitives underneath are fine to use directly for anything
  shadcn doesn't cover.
- Icons: a real icon library only — `lucide-react`, `@phosphor-icons/react`,
  `@radix-ui/react-icons`, or `@tabler/icons-react`. Pick one family and use
  it everywhere. **Never use emoji as UI chrome** (nav icons, buttons,
  status indicators, placeholders). Emoji in actual user-generated content
  (e.g. a chat message) is fine; emoji standing in for an icon is not.
- Fonts: `next/font`, never a runtime `<link>` to Google Fonts.
- Animation: Motion (`motion/react`) for anything beyond a CSS transition.

## Visual assets — this is the #1 tell of unfinished work

- **Never fake missing artwork with a flat colored `<div>`/SVG rectangle
  and an emoji or icon centered on it.** This reads as a placeholder, not
  a product, on first glance. If there is no real image, use a proper
  generated/textured placeholder (a subtle gradient + noise, a blurred
  duotone, or an actual generated image) — not a flat solid fill.
- Never build a fake product preview (fake dashboard, fake list, fake
  terminal) out of styled `<div>` rectangles to simulate a screenshot.
- Every card/tile that represents real content (a track, a user, a file)
  needs actual visual texture — a photo, generated art, or a considered
  gradient system — not one of six flat brand colors repeated.

## Depth, elevation, and materiality

- Nothing should sit on a single flat plane. Use real elevation: subtle
  shadows or borders to separate layers (sidebar vs. content vs. modal vs.
  player bar / toolbar), not just background-color differences.
- Interactive elements need real states: hover, active/pressed, focus-visible
  (a real focus ring, not `outline: none`), and disabled — all visually
  distinct, all present in the actual CSS, not assumed.
- Loading states use real skeletons (shimmer/pulse placeholders shaped like
  the content that will replace them), not a blank area or a spinner alone.

## Typography

- A real type scale (at minimum: display, heading, body, caption/label),
  not just two font-sizes. Consistent line-height and weight per level.
- Do not default to the browser's plain system sans with no weight
  variation as the only typographic decision made.

## Layout discipline

- Consistent spacing scale (an actual 4px/8px-based scale via Tailwind
  tokens), not ad hoc pixel values scattered per component.
- Micro-interactions and transitions on state changes (view switches,
  hover, expand/collapse) — a static UI with instant, jarring state
  changes reads as unfinished.
- Empty states (no results, no items yet) need real content — an icon,
  a short message, an action — not a blank area.

## AI-generated-slop tells to actively avoid

- Generic placeholder names/content that scream "demo data" without any
  attempt at realism.
- Perfectly round numbers everywhere (`100%`, `50 users`) instead of
  organic-looking real data.
- The em-dash (—) as a stylistic crutch in UI copy — use a period or comma.
- Overusing centered-everything layouts with no asymmetry or visual
  rhythm.

## Before declaring the task complete

Take a real screenshot of every major view and look at it critically
against this checklist before running final verification. If you would
be embarrassed to show this screen to a designer, it is not done —
go back and fix the specific violation, don't just re-run tests.
