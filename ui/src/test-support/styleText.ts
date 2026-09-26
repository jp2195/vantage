import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const SRC = resolve(dirname(fileURLToPath(import.meta.url)), '..')

/**
 * The declarations of every rule in a component's `<style>` blocks whose
 * selector is exactly `selector`, top-level or inside an @media block, with
 * comments stripped and whitespace collapsed. Empty when there is none.
 *
 * jsdom applies no stylesheet a single-file component carries, so a layout
 * rule that keeps a page from scrolling sideways on a phone is invisible to
 * a mounted test. What a test can hold is that the rule is still written.
 * styles/tokens.test.ts reads `<style>` blocks for the same reason.
 *
 * `file` is relative to src/, e.g. 'components/ScopePicker.vue'.
 */
export function rulesOf(file: string, selector: string): string[] {
  const source = readFileSync(resolve(SRC, file), 'utf8')
  const css = [...source.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)]
    .map((m) => m[1])
    .join('\n')
    .replace(/\/\*[\s\S]*?\*\//g, '')
  const want = selector.replace(/\s+/g, ' ').trim()
  return [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .filter((m) => m[1].replace(/\s+/g, ' ').trim() === want)
    .map((m) => m[2].replace(/\s+/g, ' ').trim())
}

/**
 * Every declaration those rules make, one `property: value` string each, as
 * written: `declarationsOf('components/DumpStateMark.vue', '.mark')` holds
 * 'display: inline-block'.
 */
export function declarationsOf(file: string, selector: string): string[] {
  return rulesOf(file, selector).flatMap((body) =>
    body
      .split(';')
      .map((d) => d.trim())
      .filter(Boolean),
  )
}
