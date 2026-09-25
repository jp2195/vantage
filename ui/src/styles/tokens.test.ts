import { readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

// "Design tokens come from tokens.css; raw hex in a component is a defect" has
// been a stated rule of this project from early on -- and it
// was violated in five components before anything checked. That is the same
// failure the closed-set column guard exists to prevent one layer up: a rule
// written in prose, enforced by nobody, discovered later only by reading
// carefully. A rule worth stating is worth failing a test over.
//
// tokens.css itself is exempt: it IS the palette, so it is the one file whose
// job is to contain literal colors.

const SRC = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const PALETTE = join(SRC, 'styles', 'tokens.css')

function styledFiles(dir: string): string[] {
  const found: string[] = []
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) {
      found.push(...styledFiles(full))
    } else if (full.endsWith('.vue') || (full.endsWith('.css') && full !== PALETTE)) {
      found.push(full)
    }
  }
  return found
}

/** `<style>` block contents for a .vue file; the whole text for a .css file. */
function styleText(file: string): string {
  const source = readFileSync(file, 'utf8')
  if (!file.endsWith('.vue')) return source
  return [...source.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)].map((m) => m[1]).join('\n')
}

// Only inside a declaration (after a `:`), so an id selector like `#abc` is not
// mistaken for a color. Comments are stripped first so a hex quoted in prose --
// explaining why a token exists, say -- does not fail the file that documents it.
const DECLARED_COLOR = /:[^;{}]*?(#[0-9a-fA-F]{3,8}\b|\b(?:white|black)\b)/g

describe('design tokens', () => {
  it('are the only source of color in components', () => {
    const violations: string[] = []
    for (const file of styledFiles(SRC)) {
      const css = styleText(file).replace(/\/\*[\s\S]*?\*\//g, '')
      const lines = css.split('\n')
      lines.forEach((line, i) => {
        for (const match of line.matchAll(DECLARED_COLOR)) {
          violations.push(`${relative(SRC, file)} (style line ${i + 1}): ${match[1]} in "${line.trim()}"`)
        }
      })
    }

    expect(
      violations,
      `Raw color literals found in components. Use a token from styles/tokens.css, ` +
        `or add one there if the palette has no name for this color yet:\n  ` +
        violations.join('\n  '),
    ).toEqual([])
  })

  it('defines every token the components reference', () => {
    const palette = readFileSync(PALETTE, 'utf8')
    const defined = new Set([...palette.matchAll(/^\s*(--[\w-]+):/gm)].map((m) => m[1]))

    const missing = new Set<string>()
    for (const file of styledFiles(SRC)) {
      for (const match of styleText(file).matchAll(/var\((--[\w-]+)\)/g)) {
        if (!defined.has(match[1])) missing.add(`${relative(SRC, file)}: ${match[1]}`)
      }
    }

    // A var() naming a token that does not exist renders as nothing -- no error,
    // no fallback, just an unset property that reads as a browser default. That
    // is the same shape as an unset color reading as black: invisible until
    // someone looks at the right pixel in the right theme.
    expect([...missing], `var() references with no definition in tokens.css`).toEqual([])
  })

  // WCAG AA asks 4.5:1 for text under 18px, and every --muted use is small
  // text: notes, column labels, quiet lines, and the neutral chips
  // ("dumping", "provisional", unspecified kinds). Those sit on --surface
  // (cards, panels, the page), --surface-2 (column labels, notes, hovered
  // rows) and --neutral-chip (the chips). At #6f7686 it measured 4.36:1 on
  // --surface-2, the note and column-label background, and 4.07:1 on
  // --neutral-chip.
  it('keeps --muted text at AA contrast on every background it sits on', () => {
    const palette = readFileSync(PALETTE, 'utf8')
    const token = (name: string): string => {
      const m = palette.match(new RegExp(`^\\s*${name}:\\s*(#[0-9a-fA-F]{6})\\s*;`, 'm'))
      if (!m) throw new Error(`${name} is not a six-digit hex in tokens.css`)
      return m[1]
    }
    const luminance = (hex: string): number => {
      const [r, g, b] = [1, 3, 5].map((i) => {
        const c = parseInt(hex.slice(i, i + 2), 16) / 255
        return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
      })
      return 0.2126 * r + 0.7152 * g + 0.0722 * b
    }
    const contrast = (a: string, b: string): number => {
      const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
      return (hi + 0.05) / (lo + 0.05)
    }
    // Guard on the arithmetic itself: --ink-2 on --neutral-chip, the stale
    // pill's pair, measured 9.25:1 in a browser.
    expect(contrast(token('--ink-2'), token('--neutral-chip'))).toBeCloseTo(9.25, 1)

    const muted = token('--muted')
    const failing = ['--surface', '--surface-2', '--neutral-chip']
      .map((bg) => [bg, contrast(muted, token(bg))] as const)
      .filter(([, ratio]) => ratio < 4.5)
      .map(([bg, ratio]) => `${bg} ${ratio.toFixed(2)}:1`)
    expect(failing).toEqual([])
  })

  // A data URI cannot read a custom property, so the select caret's stroke
  // is a literal copy of --muted. When the token moves, the caret must move
  // with it, or every dropdown keeps the old gray.
  it('draws the select caret in --muted', () => {
    const palette = readFileSync(PALETTE, 'utf8')
    const muted = palette.match(/^\s*--muted:\s*(#[0-9a-fA-F]{6})\s*;/m)?.[1]
    expect(muted).toBeDefined()
    const strokes = [...palette.matchAll(/stroke='%23([0-9a-fA-F]{6})'/g)].map((m) => `#${m[1]}`)
    expect(strokes).toHaveLength(1) // guard: the caret is the one inline SVG
    expect(strokes[0].toLowerCase()).toBe(muted!.toLowerCase())
  })
})
