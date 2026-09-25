<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import PathGraph from '@/components/PathGraph.vue'
import ScreenHeader from '@/components/ScreenHeader.vue'
import ResultMeta from '@/components/ResultMeta.vue'
import ScopePicker from '@/components/ScopePicker.vue'
import { useAsNames, useTopology, type TopologyScope } from '@/api/queries'
import {
  asNamesNotice,
  asNamesPublishedLabel,
  asNamesTruncationNotice,
  indexAsNames,
  resolveAsName,
} from '@/lib/asname'
import { trimGraph } from '@/lib/trimGraph'
import type { Graph, TopologyFanout } from '@/api/generated'

/**
 * The AS-path graph one scope's routes form.
 *
 * Everything on this screen comes from a single /v1/topology answer. Depth,
 * Min paths, the window and Re-layout are all views of a graph already in the
 * browser -- the server returns the COMPLETE graph for the scope, so none of
 * them is a parameter and none is a refetch. Only the scope control asks the
 * server anything.
 */

/** Three scope modes, which map onto the endpoint's parameters
 *  with nothing left over: a prefix (or a covering address), an AS (as origin
 *  or anywhere in the path), or a router and peer together. */
type Mode = 'prefix' | 'asn' | 'peer'

/** Keyed off the generated fanout rather than hand-typed, so the screen's idea
 *  of "the families" is the contract's. */
type Family = keyof TopologyFanout

const FAMILIES: { key: Family; label: string }[] = [
  { key: 'unicast', label: 'Unicast' },
  { key: 'vpn', label: 'VPN' },
  { key: 'evpn', label: 'EVPN' },
]

/**
 * The windows the "first seen" coloring is measured against.
 *
 * PathGraph colors an edge by whether its `first_seen` falls inside a window,
 * and its legend says so -- so the window has to be NAMED on screen, or the
 * screen is coloring edges against a bound the operator was never told. That
 * is the same reason /v1/events' and /v1/routes/history's `since` are sent
 * explicitly rather than left to a server default nobody could see.
 *
 * Hours rather than a duration string, because this bound never reaches the
 * server: the edge's timestamp is already in the response and the comparison
 * happens in the browser.
 */
const WINDOWS = [
  { value: 1, label: 'last hour' },
  { value: 6, label: 'last 6 hours' },
  { value: 24, label: 'last 24 hours' },
  { value: 168, label: 'last 7 days' },
]

const mode = ref<Mode>('prefix')
const term = ref('')
const covers = ref(false)
const inPath = ref(false)

const family = ref<Family>('unicast')
const windowHours = ref(24)
const seed = ref(0)
const selectedAsn = ref<number | undefined>(undefined)

// Held as whatever the input carries, and converted where it is read. An
// empty field means "no trim", which is a different thing from 0 -- depth 0
// is a real answer (the origins alone), so a control that turned a cleared
// field into 0 would silently narrow the graph to a single ring.
//
// Both types, because both really arrive. `v-model` on an <input
// type="number"> coerces through Vue's own looseToNumber whether or not the
// `.number` modifier is written, so a filled field lands here as a NUMBER,
// while a cleared one lands as '' and a value the browser will not parse
// lands as the raw string. A ref typed `string` compiled fine and threw
// `v.trim is not a function` the first time a test typed into the field.
const depthInput = ref<number | string>('')
const weightInput = ref<number | string>('')

const depth = computed(() => asCount(depthInput.value))
const minWeight = computed(() => asCount(weightInput.value))

function asCount(v: number | string): number | undefined {
  if (typeof v === 'number') return Number.isFinite(v) ? v : undefined
  const t = v.trim()
  if (t === '') return undefined
  const n = Number(t)
  return Number.isFinite(n) ? n : undefined
}

const scope = ref<TopologyScope | undefined>(undefined)
const { data, error } = useTopology(scope)

// The mode is the question's SHAPE, so changing it invalidates the question
// rather than reinterpreting it. Without this, switching to "from a peer"
// would leave the previous prefix's graph on screen under a picker that names
// no peer -- an answer to a question the screen is no longer asking.
watch(mode, () => {
  scope.value = undefined
  selectedAsn.value = undefined
})

function submit() {
  const v = term.value.trim()
  if (!v) return
  if (mode.value === 'prefix') {
    // Never both: prefix= and covers= are mutually exclusive on this endpoint
    // (a request carrying both is a 400), so the checkbox replaces the
    // parameter rather than adding one.
    scope.value = covers.value ? { covers: v } : { prefix: v }
  } else {
    const asn = Number(v)
    // origin_asn= is "what this AS originates"; through_asn= is "what crosses
    // it, origin included". Two different questions, and for a transit-only
    // AS the first answers with nothing.
    scope.value = inPath.value ? { through_asn: asn } : { origin_asn: asn }
  }
  selectedAsn.value = undefined
}

/**
 * ScopePicker's answer, minus the session.
 *
 * The picker resolves a session because the RIB walk it was built for pins
 * its cursor to one. /v1/topology is not pinned to a session -- it joins the
 * current one server-side, the same way /v1/routes does -- so sending one
 * would be a parameter the contract does not define. router= and peer=
 * TOGETHER are this endpoint's declared departure from /v1/routes, where the
 * same pair alone is a 400; either half on its own is half a scope here too,
 * and the picker only ever emits both or neither.
 */
function onPeerScope(s: { router: string; peer: string; session: string } | undefined) {
  scope.value = s ? { router: s.router, peer: s.peer } : undefined
  selectedAsn.value = undefined
}

/**
 * The family on screen -- one of three, never a merge of them.
 *
 * Merging the three would assert that an EVPN adjacency and a unicast
 * adjacency are the same kind of edge, which no BGP speaker claims and which
 * nothing in the response supports. The contract keys them separately for
 * that reason and the screen shows one at a time.
 */
const graph = computed<Graph | undefined>(() => data.value?.data[family.value])

const familyLabel = computed(() => FAMILIES.find((f) => f.key === family.value)?.label ?? '')

/**
 * The graph the canvas actually draws: the family's, minus what Depth and
 * Min paths trim.
 *
 * The same function PathGraph trims with (@/lib/trimGraph), because the two
 * have to agree about what is on screen. Reading the untrimmed graph here
 * shipped the defect this pane is built around: a Min paths above the
 * heaviest edge strips every edge and then every node adjacent to one, and
 * the screen drew a blank canvas with "5 ASNs · 4 edges" above it and no
 * empty state at all, because the gate had asked the untrimmed graph.
 */
const drawn = computed<Graph | undefined>(() =>
  graph.value
    ? trimGraph(graph.value, { depth: depth.value, minWeight: minWeight.value })
    : undefined,
)

/**
 * AS holder names for the ASNs actually drawn -- the graph after Depth and
 * Min paths trim it, not the family's whole answer. Selecting any drawn
 * node has to show its name without a second request, and nothing outside
 * the drawn set can be selected in the first place (PathGraph draws no
 * other nodes), so there is nothing to gain by naming a wider batch.
 */
const asnsOnScreen = computed(() => (drawn.value?.nodes ?? []).map((n) => n.asn))
const asNamesQuery = useAsNames(asnsOnScreen)
const asNameIndex = computed(() => indexAsNames(asNamesQuery.data.value?.data))

/** The once-per-screen fact, never once per node -- see @/lib/asname.ts. */
const asNamesLoadNotice = computed(() => asNamesNotice(asNamesQuery.data.value?.meta))
const asNamesDate = computed(() => asNamesPublishedLabel(asNamesQuery.data.value?.meta))
/**
 * The second once-per-screen fact. Depth and Min paths bound `drawn.nodes`
 * far below the cap at ordinary settings, but nothing here ENFORCES that:
 * a wide family at full depth is a graph, not a fleet, and the rail's
 * selected-node name is exactly where an un-looked-up ASN would read as an
 * unlisted one.
 */
const asNamesTruncNotice = computed(() =>
  asNamesTruncationNotice(asNamesQuery.data.value?.meta, asNamesQuery.truncated.value),
)

function holderName(asn: number): string | undefined {
  return resolveAsName(asNameIndex.value, asn)
}

const hasAny = (g: Graph | undefined) => Boolean(g && (g.nodes.length > 0 || g.edges.length > 0))

/**
 * Two different facts, and they must not render alike.
 *
 * An empty FAMILY is the server's answer about the network: this scope
 * reaches nothing in VPN, and no control of the operator's changes that. An
 * empty TRIM is the operator's own doing, is undone by moving a control
 * back, and says nothing about the network at all. One message for both
 * would tell half the operators who see it the wrong thing.
 */
const emptyFamily = computed(() => graph.value !== undefined && !hasAny(graph.value))
const emptyTrim = computed(() => hasAny(graph.value) && !hasAny(drawn.value))

/**
 * "5 matched routes", or "1 matched route".
 *
 * Undefined when the server sent no count, so the sentence is absent rather
 * than rendered around a blank. One is not a rare case to be tidy about: an
 * exact prefix is this screen's default mode and routinely matches exactly
 * one route, so the singular is what an operator sees the first time they
 * look something up.
 */
const matchedRoutes = computed(() => {
  const n = data.value?.meta.total_matched
  if (n === null || n === undefined) return undefined
  return `${n} matched ${n === 1 ? 'route' : 'routes'}`
})

/** `4 edges`, or `4 edges, 1 drawn` when a trim is hiding some of them. */
function count(total: number, shown: number, one: string, many: string): string {
  const noun = total === 1 ? one : many
  return total === shown ? `${total} ${noun}` : `${total} ${noun}, ${shown} drawn`
}

/**
 * The selected AS, resolved against what is DRAWN rather than held as a node
 * object.
 *
 * Two ways a selection stops being about the picture on screen: the operator
 * switches family (the three are different graphs and routinely share no ASN
 * at all), or a trim removes the node. Resolving it here covers both, so the
 * rail can never describe an AS the canvas beside it does not draw.
 */
const selected = computed(() => drawn.value?.nodes.find((n) => n.asn === selectedAsn.value))

/**
 * Every adjacency the selected AS touches, in either direction.
 *
 * Both directions, because an AS path is ordered and an AS is the `src` of
 * one edge and the `dst` of another: a list that followed `src` alone would
 * drop half of a node's adjacencies without saying so.
 */
const selectedEdges = computed(() => {
  const asn = selected.value?.asn
  if (asn === undefined) return []
  return (drawn.value?.edges ?? []).filter((e) => e.src === asn || e.dst === asn)
})

function select(asn: number) {
  selectedAsn.value = asn
}
</script>

<template>
  <section class="screen">
    <!-- The eyebrow reads "ALL COLLECTORS · LIVE RIB": every
         answer on this screen is current per-identity state, never a window
         over history, and the graph's own note says the same at length. -->
    <ScreenHeader title="AS paths" :eyebrow="['all collectors', 'live rib']" />

    <form class="scope" @submit.prevent="submit()">
      <div class="modes">
        <button
          type="button"
          data-mode="prefix"
          :class="{ on: mode === 'prefix' }"
          @click="mode = 'prefix'"
        >
          For a prefix
        </button>
        <button type="button" data-mode="asn" :class="{ on: mode === 'asn' }" @click="mode = 'asn'">
          Around an AS
        </button>
        <button
          type="button"
          data-mode="peer"
          :class="{ on: mode === 'peer' }"
          @click="mode = 'peer'"
        >
          From a peer
        </button>
      </div>

      <template v-if="mode !== 'peer'">
        <input
          v-model="term"
          class="term mono"
          :placeholder="mode === 'prefix' ? '10.10.1.0/24' : '65001'"
        />
        <label v-if="mode === 'prefix'"><input v-model="covers" type="checkbox" /> covering</label>
        <label v-else><input v-model="inPath" type="checkbox" /> in path</label>
        <button type="submit">Draw</button>
      </template>
    </form>

    <!-- router= and peer= together are a whole scope on this endpoint, which
         is exactly the pair this picker resolves. -->
    <!-- pins-collector is false: /v1/topology takes /v1/routes' scope
         parameters and none of them is a collector. It needs no pin, which
         is a stronger position than being unable to offer one: since
         2026-09-20 every count here deduplicates on the route identity with the
         collector removed, so a dual-homed router's graph is not doubled. -->
    <ScopePicker v-if="mode === 'peer'" :pins-collector="false" @scope="onPeerScope">
      <template #ambiguous>
        Both collectors' observations feed this one graph, which counts route
        identities rather than collector copies — so nothing here is doubled
        by the second collector.
      </template>
    </ScopePicker>

    <!-- Nothing is answered until something is asked. /v1/topology refuses an
         unscoped request outright -- its 400 says the alternative "would draw
         every route in the archive as one graph" -- and an empty graph pane
         rendered before a scope exists would claim this scope reaches no AS,
         about a question nobody asked. -->
    <p v-if="!scope" class="quiet">
      Name a prefix, an AS, or a router and peer, to draw the graph their routes form.
    </p>

    <template v-else>
      <p v-if="error" class="error" role="alert">{{ error.message }}</p>
      <!-- Not gated on isPending. A scope is set and nothing failed, so the
           only honest thing to say about "no answer yet" is that one is
           coming; gating this on the flag left a fifth state -- silence --
           for the combination where the composable is not yet calling itself
           pending. The screen claims four states that cannot be confused
           with each other, and a blank pane is not one of them. -->
      <p v-else-if="!graph" class="quiet">loading…</p>

      <template v-else-if="graph">
        <div class="families">
          <button
            v-for="f in FAMILIES"
            :key="f.key"
            type="button"
            :data-family="f.key"
            :class="{ on: family === f.key }"
            @click="family = f.key"
          >
            {{ f.label }}
          </button>
        </div>

        <div class="controls">
          <label>
            Depth
            <input v-model="depthInput" data-depth type="number" min="0" placeholder="all" />
          </label>
          <!-- "Min paths", not "Weight". The field behind it is
               an edge's `routes` -- paths carried in this answer, not
               capacity, preference, stability or health. A control labeled
               "Weight" hands an operator the capacity reading at the
               moment they use it, with the legend's denial sitting below
               the canvas afterwards. The prop stays `minWeight`: that is
               code. -->
          <label>
            Min paths
            <input v-model="weightInput" data-min-weight type="number" min="1" placeholder="all" />
          </label>
          <label>
            Window
            <select v-model.number="windowHours" data-window>
              <option v-for="win in WINDOWS" :key="win.value" :value="win.value">
                {{ win.label }}
              </option>
            </select>
          </label>
          <!-- The layout is deterministic, which is what lets an operator
               compare two loads of one graph -- and which also means a graph
               that settles into an unreadable arrangement settles into the
               same one every time. This re-seeds PathGraph's starting
               positions, which is the only nondeterminism the simulation
               has. -->
          <button type="button" data-relayout @click="seed++">Re-layout</button>
        </div>

        <!-- The family's own two counts. A third number like
             "13 paths · 6 ASNs · 8 edges" cannot go here: meta.total_matched
             is summed ACROSS the three families (api/topology.go, where it
             is computed), so it describes a different population than the
             ASNs and edges next to it. It is stated below instead, naming
             its own population. -->
        <p class="summary" data-summary>
          <strong>
            {{ count(graph.nodes.length, drawn?.nodes.length ?? 0, 'AS', 'ASNs') }} ·
            {{ count(graph.edges.length, drawn?.edges.length ?? 0, 'edge', 'edges') }}
          </strong>
          in the {{ familyLabel }} graph
        </p>
        <p v-if="matchedRoutes" class="matched" data-matched>
          {{ matchedRoutes }}, across all three families — the population all three graphs were
          built from, counted once per route identity.
        </p>

        <!-- The AS holder-name dataset's own state, said ONCE for the whole
             graph -- never once per node in the rail below. The two
             branches are mutually exclusive facts, and neither prints
             before the batch has answered: no answer yet is not "not
             loaded" -- see @/lib/asname.ts. -->
        <p v-if="asNamesLoadNotice" class="quiet" data-asnames-notice>{{ asNamesLoadNotice }}</p>
        <p v-else-if="asNamesDate" class="quiet" data-asnames-date>
          AS holder names as of {{ asNamesDate }}.
        </p>
        <!-- A SECOND line, not an alternative to the two above: a loaded
             dataset and a graph wider than one lookup are both true at
             once, and only the first is otherwise said out loud. -->
        <p v-if="asNamesTruncNotice" class="quiet" data-asnames-truncated>
          {{ asNamesTruncNotice }}
        </p>

        <div class="pane">
          <div class="canvas">
            <PathGraph
              v-if="drawn && !emptyFamily && !emptyTrim"
              :graph="graph"
              :depth="depth"
              :min-weight="minWeight"
              :window-hours="windowHours"
              :seed="seed"
              @select="select"
            />
            <!-- Not an alert, and worded so it cannot be read as one: an
                 empty family is ordinary. `nodes` and `edges` are arrays with
                 no null form, so an empty one is this API's positive claim
                 "nothing there" -- and a blank 580x460 canvas says neither
                 that nor anything else, it just reads as a screen that broke.
                 The adjacent case is a SMALL graph, which is drawn and states
                 its counts above: an operator who cannot tell small from
                 empty has been told the wrong thing. -->
            <p v-else-if="emptyFamily" class="empty" data-empty-family>
              No {{ familyLabel }} adjacency in this scope. The answer carried an empty graph for
              this family, which this API defines as a positive “nothing there” rather than “we did
              not look” — the other two families are unaffected.
            </p>
            <!-- The other empty, and deliberately not the same sentence. This
                 one is the operator's own controls, it says nothing about the
                 network, and it is undone by moving them back. -->
            <p v-else class="empty" data-empty-trim>
              Depth and Min paths have trimmed this whole graph out of the picture. Both filter the
              answer already in the browser rather than asking the server for less, so widening
              either one draws it straight back.
            </p>
          </div>

          <aside class="rail" data-rail>
            <template v-if="selected">
              <!-- The AS number heading stays bare, exactly like every
                   graph node beside it: the full registered-holder line
                   does not fit a node box or a heading either, so it is a
                   separate line below rather than appended here. Absent
                   entirely, rather than a fabricated one, when the AS
                   holder-name dataset has nothing for this AS or is not
                   loaded at all -- see @/lib/asname.ts. -->
              <h2 class="mono">AS{{ selected.asn }}</h2>
              <p v-if="holderName(selected.asn)" class="holder" data-holder-name>{{
                holderName(selected.asn)
              }}</p>
              <dl class="fields">
                <div class="field">
                  <dt data-node-field="roles">Roles</dt>
                  <dd>{{ selected.roles.join(' · ') }}</dd>
                </div>
                <div class="field">
                  <dt data-node-field="routes">Paths through it</dt>
                  <dd>{{ selected.routes }}</dd>
                </div>
                <div class="field">
                  <dt data-node-field="first_seen">First seen</dt>
                  <dd class="mono">{{ selected.first_seen }}</dd>
                </div>
              </dl>
              <!-- Both numbers say what population they are over, because
                   neither is a fact about the AS. Paths counts the matched
                   routes, and first_seen dates THOSE routes: a route
                   re-advertised with a new path brings its original timestamp
                   to whatever AS it traverses now, so a recent one is not a
                   claim that the AS is new to the graph. The endpoint's own
                   schema says a client deriving "newly appeared" from it can
                   be wrong in both directions. -->
              <p class="says">
                Both are over the routes in this answer, not over the AS: Paths counts a route once
                however often its path repeats the ASN, and First seen is the earliest collection
                time among those routes, bounded by the archive's history retention (90 days by default).
              </p>

              <h3>Adjacencies</h3>
              <p v-if="selectedEdges.length === 0" class="quiet">
                No adjacency in this answer touches AS{{ selected.asn }}. An AS reached in one hop
                contributes a node and no edge.
              </p>
              <table v-else class="edges">
                <thead>
                  <tr>
                    <th data-edge-field="src">From</th>
                    <th data-edge-field="dst">To</th>
                    <th data-edge-field="routes">Paths</th>
                    <th data-edge-field="live_routes">Live</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="e in selectedEdges"
                    :key="`${e.src}-${e.dst}`"
                    data-edge-row
                    :data-edge="`${e.src}-${e.dst}`"
                    :class="{ withdrawn: e.live_routes === 0 }"
                  >
                    <td class="mono">AS{{ e.src }}</td>
                    <td class="mono">AS{{ e.dst }}</td>
                    <td class="num">{{ e.routes }}</td>
                    <td class="num">{{ e.live_routes }}</td>
                  </tr>
                </tbody>
              </table>
              <p class="says">
                Paths is how many of the matched routes carry that adjacency; Live is how many of
                them are not withdrawn. 0 live means every route carrying it has been withdrawn —
                the collector knows the edge and the network no longer uses it.
              </p>
            </template>
            <!-- Only where there is something to select. An empty family
                 draws no node, and an instruction that cannot be followed is
                 one more thing on screen that is not true. -->
            <p v-else-if="drawn && drawn.nodes.length > 0" class="quiet">
              Select an AS in the graph to see its roles and the adjacencies it carries.
            </p>
          </aside>
        </div>

        <ResultMeta v-if="data" :meta="data.meta" />
      </template>
    </template>
  </section>
</template>

<style scoped>
.screen { padding: 20px 24px; display: flex; flex-direction: column; gap: 12px; }
h1 { margin: 0; font: 600 17px var(--font-ui); color: var(--ink); }
.scope { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.modes, .families { display: flex; gap: 2px; }
.modes button, .families button {
  font: 500 11.5px var(--font-ui);
  padding: 5px 11px;
  border: 1px solid var(--line-2);
  background: var(--surface);
  color: var(--muted);
  border-radius: 6px;
  cursor: pointer;
}
.modes button.on, .families button.on { background: var(--ink); color: var(--on-dark); border-color: var(--ink); }
.scope input.term { border: 1px solid var(--line-2); border-radius: 6px; padding: 6px 10px; font-size: 12px; min-width: 220px; }
.scope label { font: 400 11.5px var(--font-ui); color: var(--muted); display: flex; gap: 5px; align-items: center; }
.scope button[type="submit"] { background: var(--ink); color: var(--on-dark); border: 0; border-radius: 6px; padding: 7px 13px; font: 500 11.5px var(--font-ui); cursor: pointer; }

.controls { display: flex; gap: 14px; align-items: center; flex-wrap: wrap; }
.controls label { display: flex; gap: 6px; align-items: center; font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
.controls input, .controls select { font: 400 12px var(--font-ui); padding: 5px 8px; border: 1px solid var(--line-2); border-radius: 6px; background: var(--surface); color: var(--ink); text-transform: none; letter-spacing: normal; }
.controls input { width: 72px; }
.controls button { font: 500 11.5px var(--font-ui); padding: 5px 11px; border: 1px solid var(--line-2); background: var(--surface); color: var(--muted); border-radius: 6px; cursor: pointer; }

.summary { margin: 0; font: 400 12px var(--font-ui); color: var(--muted); }
.summary strong { color: var(--ink); font-weight: 600; }
.matched { margin: 0; font: 400 11px var(--font-ui); color: var(--muted); }

/* The rail is a fixed 300px and the canvas took whatever was left, down to
   nothing: `min-width: 0` let it shrink past its own content, so below about
   a 430px viewport the graph was 0 wide and the legend's four children --
   which cannot shrink below their words -- painted straight over the rail's
   text. Measured in a browser against these exact declarations: six
   overlapping text pairs, worst overlap 19px.

   This is NOT a responsive design, and this app has no @media rule anywhere:
   a narrow viewport is unsupported here as it is on every other screen. What
   changes is only that the row stops shrinking and wraps instead, so a
   squeezed layout DEGRADES rather than becoming illegible.

   The 500px floor is measured rather than picked. PathGraph draws in a
   580-unit viewBox scaled to the pane, so its smallest text -- the 10.5-unit
   role line -- renders at 10.5 x pane / 580 CSS px: 9.1px at a 500px pane,
   which is the size of the smallest text elsewhere in this app, and 4.1px at
   the 225px pane a 640px viewport used to leave. Measured after the change,
   role text never falls below 9.1px at any width.

   The desktop layout is untouched: measured byte-identical at 1440, 1280,
   1024, 1000 and 920 (canvas 1025 / 865 / 609 / 585 / 505px in both). The
   rail wraps at 914 and below, which is where 500 + 16 gap + 317 rail no
   longer fits the pane's content box. */
.pane { display: flex; flex-wrap: wrap; gap: 16px; align-items: flex-start; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); padding: 14px 16px; }
.canvas { flex: 1; min-width: 500px; }
.rail { width: 300px; flex: none; border-left: 1px solid var(--divider); padding-left: 16px; }
.rail h2 { margin: 0 0 4px; font: 600 13px var(--font-data); color: var(--ink); }
.rail .holder { margin: 0 0 8px; color: var(--muted); font: 400 11px var(--font-ui); }
.rail h3 { margin: 14px 0 6px; font: 600 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; }
.fields { margin: 0; display: flex; flex-direction: column; gap: 5px; }
.field { display: flex; gap: 8px; align-items: baseline; }
.field dt { font: 500 10.5px var(--font-ui); color: var(--muted); min-width: 108px; }
.field dd { margin: 0; font: 400 11.5px var(--font-ui); color: var(--ink-2); }
.field dd.mono { font-family: var(--font-data); font-size: 10.5px; }

.edges { width: 100%; border-collapse: collapse; font-size: 11px; }
.edges th { text-align: left; font: 500 10px var(--font-ui); color: var(--muted); text-transform: uppercase; letter-spacing: .04em; padding-bottom: 4px; border-bottom: 1px solid var(--divider); }
.edges td { padding: 3px 0; color: var(--ink-2); border-bottom: 1px solid var(--line-faint); }
.edges td.mono { font: 400 10.5px var(--font-data); }
.edges td.num { text-align: right; font: 400 10.5px var(--font-data); }
.edges tr.withdrawn td { color: var(--bad-2); }

.empty { margin: 0; padding: 16px 0; color: var(--muted); font: 400 11.5px var(--font-ui); max-width: 46em; }
.says { margin: 8px 0 0; color: var(--muted); font: 400 10.5px var(--font-ui); }
.quiet { margin: 0; color: var(--muted); font: 400 11.5px var(--font-ui); }
.error { margin: 0; color: var(--bad-2); font: 400 11.5px var(--font-ui); }
</style>
