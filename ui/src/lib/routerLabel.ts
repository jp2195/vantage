/**
 * What to render in a Router cell when the router never told us its name.
 *
 * A router's sysName arrives in the BMP Initiation message (RFC 7854 §4.4),
 * and a router that never sends one -- or sends one with no sysName TLV --
 * leaves `router_sysname` as the empty string. That is a real, expected
 * value, not a gap: the collector accepts such a session by design (see
 * collector/session.go, "a router that never sends one (or sends it late)
 * is still the device the operator says it is"), two routers in the live
 * archive report it, and every capture in the corpus's EVPN set sends no
 * Initiation at all.
 *
 * Rendered raw it is a BLANK CELL in an identity column, which an operator
 * cannot tell from a rendering fault. Found in a browser on 2026-09-20:
 * four table screens did exactly that while MonitorView, one screen over,
 * said "(no sysName TLV)" for the same router.
 *
 * The notice rather than the address, even though ScopePicker renders
 * `(10.0.0.1)` for the same condition: that control has no address column
 * beside it and this text is used in tables that do, so repeating the
 * address would fill an identity column with a copy of its neighbor. The
 * two conventions answer the same question in different room.
 *
 * The exact string matches the Grafana dashboards' own
 * `if(router_sysname = '', '(no sysName TLV)', router_sysname)`, so one
 * router reads the same in both surfaces.
 */
export function routerLabel(sysname: string | null | undefined): string {
  return sysname && sysname.length > 0 ? sysname : '(no sysName TLV)'
}
