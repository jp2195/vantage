import type { PeerEvent } from '@/api/generated'

/**
 * Whether there is a reason to show at all -- the same rule reasonDisplay
 * applies below, in one place rather than two.
 *
 * A table column has to render SOMETHING in every cell and gets the dash;
 * a feed that lays each reason on its own line has no such obligation and
 * would otherwise stack an absence as a row of dashes (MonitorView's
 * "Recent peer events"). Both need the same answer to "does this kind carry
 * one", so neither asks it by testing the dash the other returns.
 */
export function hasDownReason(row: Pick<PeerEvent, 'kind'>): boolean {
  return row.kind === 'down'
}

/**
 * down_reason beside its name -- but only for a "down".
 *
 * Extracted from SessionHistoryView.vue so the Events screen renders the
 * identical thing rather than a second copy: two screens with two copies of
 * this logic is how the two drift apart. It lives in `lib/` rather than
 * `components/` because it is not a component -- nothing here renders
 * anything -- it is display logic two Vue components both call, and `lib/`
 * is this app's home for exactly that (see also `lib/formatClock.ts`).
 *
 * A view_lost or up event's down_reason is stored as 0 because the router
 * made no statement at all, never because the reason itself was "0"; showing
 * that 0 would report an absence as if it were a code. The KIND decides, not
 * the presence of a value.
 *
 * The number is carried in BOTH remaining cases, which is the whole point of
 * the shape. /v1/events' contract is "the registry name and the number, so a
 * caller is never forced to trust the decoding" (api/types.go's reasonName,
 * and TestEventsNamesTheDownReasonBesideTheNumber); showing the name alone
 * drops the half a reader would need to check it, and showing a bare number
 * for an unregistered code puts two different notations in one column -- an
 * operator seeing "2" beside "local system closed, FSM event follows" cannot
 * tell whether 2 IS the reason or merely a code this build has no name for.
 * Saying so is the positive answer, the same way api/types.go's reasonName
 * returns "" rather than guessing.
 *
 * It takes the three fields it reads rather than a whole PeerEvent, so a
 * caller with a partial row -- a test, a future summary shape -- can use it
 * without manufacturing fields it does not need.
 */
export function reasonDisplay(
  row: Pick<PeerEvent, 'kind' | 'down_reason' | 'reason_name'>,
): string {
  if (!hasDownReason(row)) return '—'
  if (!row.reason_name) return `${row.down_reason} (no name for this code)`
  return `${row.down_reason} · ${row.reason_name}`
}
