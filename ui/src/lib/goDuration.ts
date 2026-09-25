/**
 * Parses a Go duration string -- "30m0s", "2m", "1h30m0s" -- into
 * milliseconds.
 *
 * This exists because `meta.churn_bucket` is one: the daemon formats it with
 * Go's own Duration.String(), which is the honest thing for it to send (it
 * is the value the query ran with) and not something JavaScript parses.
 * The chart needs it as a number to give a bar its true WIDTH ON A TIME
 * AXIS, which is the difference between "a 30-minute bucket" and "one bar
 * of however many there happen to be".
 *
 * Returns undefined rather than throwing or guessing on anything it does not
 * recognize: a caller that cannot learn the width should say so, not draw a
 * bar of invented duration.
 */
export function parseGoDuration(s: string): number | undefined {
  const m = s.match(/^(\d+(?:\.\d+)?h)?(\d+(?:\.\d+)?m)?(\d+(?:\.\d+)?s)?(\d+(?:\.\d+)?ms)?$/)
  if (!m || !s || s === '0') return undefined
  const [, h, min, sec, ms] = m
  if (!h && !min && !sec && !ms) return undefined
  return (
    (h ? parseFloat(h) * 3_600_000 : 0) +
    (min ? parseFloat(min) * 60_000 : 0) +
    (sec ? parseFloat(sec) * 1000 : 0) +
    (ms ? parseFloat(ms) : 0)
  )
}

/**
 * The same duration written the way a person reads one: "30m0s" becomes
 * "30 min", "1h30m0s" becomes "1 hr 30 min".
 *
 * The wire is right to send Go's own Duration.String() -- `meta.churn_bucket`
 * and `meta.activity_window` are both the value the query actually ran with,
 * and echoing it back verbatim is what makes them checkable. But "30m0s" is
 * a serialization, not a label, and a screen that prints it has leaked an
 * implementation detail onto a figcaption and into a screen reader's mouth.
 * Formatting belongs on this side of the wire for the same reason
 * formatClock's does: the value is the fact, the rendering is presentation.
 *
 * Anything parseGoDuration does not recognize comes back UNCHANGED rather
 * than as a guess or a dash -- showing exactly what the server said is the
 * honest answer when this cannot improve on it.
 */
export function formatGoDuration(s: string): string {
  const ms = parseGoDuration(s)
  if (ms === undefined) return s
  const seconds = Math.round(ms / 1000)
  const parts: string[] = []
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const rest = seconds % 60
  if (hours) parts.push(`${hours} hr`)
  if (minutes) parts.push(`${minutes} min`)
  // The seconds term appears when there are any, and also when there is
  // nothing else to say: a sub-second window must not render as "".
  if (rest || parts.length === 0) parts.push(`${rest} sec`)
  return parts.join(' ')
}
