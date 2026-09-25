/**
 * The render-or-fall-back rule for AS holder names, in one place so
 * PeersView, RoutesView and TopologyView cannot each invent their own.
 *
 * The distinction every function below exists to keep apart: "this ASN has
 * no registered holder" and "no name dataset is loaded" are different
 * facts (api/openapi.yaml's own words on GET /v1/asnames' `meta.
 * asnames_loaded`), and a caller that reads an empty `name` without
 * checking `asnames_loaded` first cannot tell them apart. Every ASN in this
 * project's own test archive is private-range and therefore genuinely
 * unlisted, so "the dataset says nothing about this row" is the ordinary
 * case here, not a broken one -- and it must never be reported as "the
 * dataset is missing" when the truth is the first.
 */
import { formatClock } from './formatClock'
import type { AsName, Meta } from '@/api/generated'

/**
 * The one sentence a screen shows when no AS holder-name dataset is loaded
 * at ALL -- printed ONCE per screen, never once per row. A table with fifty
 * unlisted rows must not repeat this fifty times just because fifty cells
 * each individually have no name to show.
 */
export const ASNAMES_NOT_LOADED_NOTICE =
  'AS holder names are not loaded on this stack. Run `make fetch-asnames` and apply the dictionary to see them.'

/**
 * The most ASNs one GET /v1/asnames may name, and therefore the most any
 * screen can have looked up in a single batch.
 *
 * It lives HERE, next to the sentence that has to state it, rather than in
 * `@/api/queries` where it is enforced: the number and the words a screen
 * shows about the number are one fact, and two copies of it drifting apart
 * would produce a notice that names a bound the code does not apply. The
 * daemon enforces the same 512 itself (`api/asnames.go`'s maxASNBatch, and
 * `asn=`'s own `maxItems` in `api/openapi.yaml`); this constant is the UI
 * side of that agreement, not a second opinion about it.
 */
export const ASN_BATCH_CAP = 512

/**
 * The second once-per-screen sentence: a screen carrying more distinct
 * ASNs than one lookup may name.
 *
 * This exists because the cap degrades DISHONESTLY without it. The lookup
 * keeps the numerically lowest ASN_BATCH_CAP ASNs, so past the cutoff every
 * high-numbered ASN renders exactly like a genuinely unlisted one -- the
 * precise collapse this whole module exists to prevent -- while the screen's
 * own date line goes on saying names are loaded as of a date. The operator
 * sees "AS3356, no name" and has no way to tell that from "AS3356 was never
 * asked about".
 *
 * It names the cutoff RULE, not just the count, because which ASNs survive
 * is what lets an operator judge their own: the lowest-numbered 512 are
 * looked up, so a legacy 16-bit ASN is likely named and a 4-byte one is
 * likely not.
 */
export const ASNAMES_TRUNCATED_NOTICE =
  `This screen carries more than ${ASN_BATCH_CAP} distinct AS numbers, and one lookup can name at most that many. ` +
  `Only the ${ASN_BATCH_CAP} lowest-numbered were looked up, so every other ASN here renders bare whether or not ` +
  `the dataset lists it. Narrow the view to name the rest.`

/** One batch answer's rows, indexed by ASN for O(1) lookup per row/node. */
export type AsNameIndex = Map<number, AsName>

/**
 * Builds the index a screen looks names up against.
 *
 * `undefined` input -- no answer has landed yet, or the request itself
 * failed -- indexes to an empty map, which resolves every ASN to "no name"
 * below: the same bare-number rendering a genuinely unlisted ASN gets, and
 * the correct one. Neither "no answer yet" nor "the request failed" is
 * grounds to claim the dataset IS loaded, so nothing here does.
 */
export function indexAsNames(rows: AsName[] | undefined): AsNameIndex {
  return new Map((rows ?? []).map((r) => [r.asn, r]))
}

/**
 * The bare-ASN-or-named decision for one row or graph node.
 *
 * Returns the WHOLE registered-holder line, exactly as the dataset carries
 * it -- never split on `" - "` or any other separator. RIPE's own list is
 * why: 31.8% of its lines carry no separator at all, 1,530 carry more than
 * one, and "the first token is the handle" fails on 9,916 more, so any
 * split guesses at a boundary this dataset does not mark (see
 * `AsName.name`'s own contract description for the identical count).
 *
 * `undefined` covers two facts a caller does not need to tell apart per
 * row: this ASN is genuinely unlisted on a loaded dataset, or the index
 * carries nothing for it at all (no dataset, or none fetched yet). Telling
 * those two apart is `asNamesNotice`'s job, once per screen -- not this
 * function's, once per row.
 */
export function resolveAsName(index: AsNameIndex, asn: number): string | undefined {
  return index.get(asn)?.name || undefined
}

/**
 * The once-per-screen notice, or `undefined` when there is nothing to warn
 * about.
 *
 * Reads `meta.asnames_loaded` alone, and ONLY when it is explicitly `false`
 * -- never inferred from an all-empty batch. A test archive built from
 * private-range addresses is the reason that distinction is load-bearing
 * rather than academic: every ASN in it is private-range, so a
 * PERFECTLY LOADED dataset answers every row here with an empty name,
 * and inferring "not loaded" from that emptiness would misreport the
 * one deployment available to verify this against.
 *
 * `undefined` meta -- no answer has landed yet, or the request failed --
 * prints nothing rather than a claim in either direction: silence until the
 * daemon has actually said "no dataset", not "we don't know yet" misread as
 * "there is no dataset".
 */
export function asNamesNotice(meta: Meta | undefined): string | undefined {
  return meta?.asnames_loaded === false ? ASNAMES_NOT_LOADED_NOTICE : undefined
}

/**
 * The truncation notice, or `undefined` when there is nothing to warn about.
 *
 * Both conditions are required, and the second is the interesting one.
 *
 * `truncated` alone is not enough: with NO dataset loaded, every ASN on the
 * screen renders bare for a reason `asNamesNotice` already states in full,
 * and the operator's next action (`make fetch-asnames`) is the same whether
 * 10 or 10,000 ASNs are on screen. Printing a second sentence about a
 * lookup that would have found nothing anyway competes with the one that
 * matters. Once the dataset IS loaded the truncation becomes the ONLY
 * remaining reason a name can be missing that the screen is not already
 * explaining -- so that is exactly when this speaks.
 *
 * `undefined` meta -- no answer yet, or the request failed -- prints
 * nothing, the same silence `asNamesNotice` keeps for the same reason: the
 * screen says nothing about the dataset until the daemon has.
 */
export function asNamesTruncationNotice(
  meta: Meta | undefined,
  truncated: boolean,
): string | undefined {
  return truncated && meta?.asnames_loaded === true ? ASNAMES_TRUNCATED_NOTICE : undefined
}

/**
 * RIPE's own publication date, formatted the way every other timestamp in
 * this app is (`formatClock`) -- never the moment `make fetch-asnames` ran,
 * per `meta.asnames_published`'s own contract description.
 *
 * `undefined` covers both reasons the field can be absent or null: no
 * dataset is loaded, or the names dictionary is loaded while its date
 * companion is not -- a real, independently-degrading shape this project
 * measured and shipped a test for (query/asnamesmeta.go). Either
 * way there is no date to show, and this must not invent one.
 */
export function asNamesPublishedLabel(meta: Meta | undefined): string | undefined {
  const iso = meta?.asnames_published
  return iso ? formatClock(iso) : undefined
}
