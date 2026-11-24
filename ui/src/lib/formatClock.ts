/** Seconds-precision wall clock, 24-hour whatever the locale's default. */
const TIME: Intl.DateTimeFormatOptions = {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hourCycle: 'h23',
}

/** The date half of a row's timestamp: "Sep 22, 2026" in en-US. */
const DATE: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'short', day: 'numeric' }

/**
 * Renders an ISO instant ("2026-09-06T19:14:28.788894Z") the way an
 * operator reads a clock, not a string with a T, a Z and six digits of
 * sub-second precision in it -- presentation, not invention.
 *
 * Shared by EventsView.vue and SessionHistoryView.vue (both render
 * ts_collector, the same column from the same peer_events source) and
 * RoutersView.vue (last_seen, a different column from a different
 * endpoint, formatted identically because an operator reading either
 * screen expects the same clock). Three copies of this one-line
 * computation is how they would quietly drift -- one screen picking up a
 * timezone or precision option the others never got -- so it lives in
 * `lib/` once, alongside `lib/eventReason.ts`, rather than in any one
 * screen.
 *
 * On a 24-hour clock, in every locale. The screens used to mix three
 * renderings of the same kind of instant: the chrome said "12:05:58 PM",
 * the table footer said "12:05:58" and a row said "Sep 22, 2026, 11:46:35
 * AM". Operators read these beside router logs and syslog, which are
 * 24-hour, and so is ChurnChart's axis.
 *
 * The year appears only when it is not the reader's current year:
 * "Nov 24, 14:54:36", but "Dec 28, 2025, 10:00:00" once the calendar has
 * turned. Retention is 90 days and an archive can go quiet across New Year,
 * so a row from last year must still say so -- but spending ", 2026" on
 * every row of a table made the time column 22 characters of the data face,
 * wider than its column, and every timestamp in Events, Peers and Session
 * history wrapped onto two lines. Same local-calendar comparison as
 * formatFreshness below, and `now` is injected for the same reason.
 */
export function formatClock(iso: string, now: Date = new Date()): string {
  const t = new Date(iso)
  const date: Intl.DateTimeFormatOptions =
    t.getFullYear() === now.getFullYear() ? { month: 'short', day: 'numeric' } : DATE
  return t.toLocaleString([], { ...date, ...TIME })
}

/**
 * The same instant as a bare wall clock -- "19:55:15" -- for the chrome's
 * updated stamp and the table footer's "complete as of", where the date is
 * noise: it is always today, and the cluster has one line of room beside
 * the nav.
 *
 * Its own function rather than an option on formatClock, because the two
 * have different jobs. formatClock renders a row's timestamp, where the
 * date is part of the fact; this renders "how fresh is what I am looking
 * at", where it is not.
 */
export function formatTimeOfDay(iso: string): string {
  return new Date(iso).toLocaleTimeString([], TIME)
}

/**
 * The newest instant the archive holds, for the chrome's freshness fact --
 * "newest row 09:41:12" today, "newest row Sep 20, 2026, 09:41:12" when it
 * is not today.
 *
 * Neither formatTimeOfDay nor formatClock on its own: the first drops the
 * date, which is exactly the information this fact exists to carry when an
 * archive has gone quiet ("newest row 09:41" beside "updated 14:03" reads
 * as four hours old when it may be four days), and the second always spends
 * a date on a cluster that shares one line with an eleven-item nav.
 *
 * `now` is injected so the behavior is testable without freezing a clock,
 * and the comparison is `toDateString`, which is LOCAL. Comparing ISO date
 * prefixes instead would be a UTC comparison and would disagree with the
 * operator's own calendar for five hours of every day here -- the same
 * local-versus-UTC conflation vite.config.ts pins the test timezone to
 * catch. This is a presentation choice about the reader's day, never a
 * claim about the data's clock: the instant itself is rendered as it
 * arrived, on the collector's clock, and is never turned into an age. An
 * age would subtract the browser's clock from the collector's, which is the
 * mixed-clock defect this project has fixed repeatedly.
 */
export function formatFreshness(iso: string, now: Date = new Date()): string {
  const t = new Date(iso)
  return t.toDateString() === now.toDateString() ? formatTimeOfDay(iso) : formatClock(iso)
}
