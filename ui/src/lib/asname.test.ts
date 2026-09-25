import { describe, expect, it } from 'vitest'
import type { AsName, Meta } from '@/api/generated'
import { formatClock } from './formatClock'
import {
  ASNAMES_NOT_LOADED_NOTICE,
  ASNAMES_TRUNCATED_NOTICE,
  ASN_BATCH_CAP,
  asNamesNotice,
  asNamesPublishedLabel,
  asNamesTruncationNotice,
  indexAsNames,
  resolveAsName,
} from './asname'

/** A complete Meta, so every test states only the fields it means to vary. */
function meta(overrides: Partial<Meta> = {}): Meta {
  return { warnings: [], total_matched: null, ...overrides }
}

describe('resolveAsName', () => {
  it('renders the bare ASN for an ASN the dataset does not list, without claiming the dataset is missing', () => {
    // A row IS present for this ASN -- the dataset was asked about it and
    // answered -- but its name is "", RIPE's own positive claim that this
    // ASN has no registered holder. resolveAsName reads the same either way
    // this function is asked about a missing row or an explicitly empty
    // one; telling "unlisted" apart from "no dataset" is asNamesNotice's
    // job below, not this function's.
    const index = indexAsNames([{ asn: 65010, name: '', country: '' }])
    expect(resolveAsName(index, 65010)).toBeUndefined()
  })

  it('renders the bare ASN when the batch never heard of this ASN at all', () => {
    const index = indexAsNames([{ asn: 3356, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' }])
    expect(resolveAsName(index, 65010)).toBeUndefined()
  })

  it('renders the bare ASN when no answer has landed at all', () => {
    expect(resolveAsName(indexAsNames(undefined), 3356)).toBeUndefined()
  })

  it('renders the whole name, not a fragment before a separator', () => {
    // RIPE's own list carries lines with zero, one, and more than one " - "
    // separator; a name is the whole registered-holder line regardless.
    const noSeparator = 'GOOGLE'
    const oneSeparator = 'LEVEL3 - Level 3 Parent, LLC'
    const twoSeparators = 'ATT-INTERNET4 - AT&T Services, Inc. - Legal Successor'
    const index = indexAsNames([
      { asn: 1, name: noSeparator, country: 'US' },
      { asn: 2, name: oneSeparator, country: 'US' },
      { asn: 3, name: twoSeparators, country: 'US' },
    ])
    expect(resolveAsName(index, 1)).toBe(noSeparator)
    expect(resolveAsName(index, 2)).toBe(oneSeparator)
    expect(resolveAsName(index, 3)).toBe(twoSeparators)
  })

  it('carries the batch as first-seen order does not matter for a keyed lookup', () => {
    const rows: AsName[] = [
      { asn: 15169, name: 'GOOGLE - Google LLC', country: 'US' },
      { asn: 3356, name: 'LEVEL3 - Level 3 Parent, LLC', country: 'US' },
    ]
    const index = indexAsNames(rows)
    expect(resolveAsName(index, 3356)).toBe('LEVEL3 - Level 3 Parent, LLC')
    expect(resolveAsName(index, 15169)).toBe('GOOGLE - Google LLC')
  })
})

describe('asNamesNotice', () => {
  it('renders the bare ASN when the dataset is not loaded, and says why once per screen', () => {
    expect(asNamesNotice(meta({ asnames_loaded: false }))).toBe(
      ASNAMES_NOT_LOADED_NOTICE,
    )
  })

  it('says nothing when a dataset IS loaded, even though every row it answered is unlisted', () => {
    // A realistic case for a private test network: every ASN is
    // private-range, so a perfectly loaded dataset answers every row
    // with an empty name. asnames_loaded alone -- never an empty batch
    // -- is what this reads.
    expect(asNamesNotice(meta({ asnames_loaded: true }))).toBeUndefined()
  })

  it('says nothing before any answer has landed, rather than guessing which fact is true', () => {
    expect(asNamesNotice(undefined)).toBeUndefined()
  })
})

describe('asNamesTruncationNotice', () => {
  // The defect this whole function exists for: with a dataset loaded and
  // the batch capped, every ASN past the cutoff renders bare -- byte for
  // byte identical to an ASN the dataset genuinely does not list -- while
  // the screen's own date line says names are loaded as of a date. Without
  // this sentence the screen is stating something false by omission.
  it('says the batch was cut short, so a bare ASN past the cap is not read as unlisted', () => {
    expect(asNamesTruncationNotice(meta({ asnames_loaded: true }), true)).toBe(
      ASNAMES_TRUNCATED_NOTICE,
    )
  })

  // The bound and the cutoff RULE both have to be in the words, because
  // "the lowest-numbered 512" is what lets an operator judge whether their
  // OWN ASN was looked up. "Some names are missing" would not.
  it('names the bound and which ASNs survived it', () => {
    expect(ASNAMES_TRUNCATED_NOTICE).toContain(String(ASN_BATCH_CAP))
    expect(ASNAMES_TRUNCATED_NOTICE).toMatch(/lowest-numbered/i)
    // And it must not reach for the other notice's claim: the dataset IS
    // loaded here, and saying otherwise collapses the two facts this
    // module exists to keep apart.
    expect(ASNAMES_TRUNCATED_NOTICE).not.toMatch(/not loaded/i)
  })

  // The half that makes the line mean something. A notice a screen shows
  // unconditionally is a notice an operator stops reading.
  it('says nothing when the batch fit', () => {
    expect(asNamesTruncationNotice(meta({ asnames_loaded: true }), false)).toBeUndefined()
  })

  // Truncated AND no dataset: both true, but only one is worth saying.
  // Every ASN on that screen is bare for a reason asNamesNotice already
  // states in full, and the operator's next move (`make fetch-asnames`) is
  // the same either way -- so a second sentence here would only compete
  // with the one that matters.
  it('defers to the not-loaded notice when there is no dataset to have been cut short', () => {
    expect(asNamesTruncationNotice(meta({ asnames_loaded: false }), true)).toBeUndefined()
  })

  // Same silence asNamesNotice keeps, for the same reason: nothing is said
  // about the dataset until the daemon has said something about it.
  it('says nothing before any answer has landed', () => {
    expect(asNamesTruncationNotice(undefined, true)).toBeUndefined()
  })
})

describe('asNamesPublishedLabel', () => {
  it('shows the dataset date beside the names', () => {
    const iso = '2026-09-18T10:49:00Z'
    expect(asNamesPublishedLabel(meta({ asnames_loaded: true, asnames_published: iso }))).toBe(
      formatClock(iso),
    )
  })

  it('shows no date when the dataset is not loaded', () => {
    expect(
      asNamesPublishedLabel(meta({ asnames_loaded: false })),
    ).toBeUndefined()
  })

  it('shows no date when the names dictionary is loaded but its date companion is not', () => {
    // A real, independently-degrading shape (query/asnamesmeta.go): the
    // names dictionary can be loaded while asnames_meta is not. The honest
    // answer is no date at all -- never a guessed or cached one -- and the
    // contract spells that as an ABSENT key rather than a present null
    // (api/types.go's ASNamesPublished carries omitempty), which is why
    // this fixture omits it instead of setting it to null.
    expect(
      asNamesPublishedLabel(meta({ asnames_loaded: true })),
    ).toBeUndefined()
  })

  it('shows no date before any answer has landed', () => {
    expect(asNamesPublishedLabel(undefined)).toBeUndefined()
  })
})
