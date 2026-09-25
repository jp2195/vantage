// Tests for the CLA check. Run with `make cla-test` (node --test).
//
// The workflow cannot be exercised against real pull requests until GitHub
// Actions runs for this repository, so everything it decides is tested here:
// the pure functions directly, and run() end to end against FakeGitHub, an
// in-memory stand-in for the slice of the REST API the check touches. Each
// test's comment names the change to cla.js that should turn it red.

const test = require('node:test');
const assert = require('node:assert/strict');
const cla = require('./cla.js');

const CLA_TEXT = `# vantage Individual Contributor License Agreement

Some preamble.

## Why this exists

Reasons.

## The agreement

### 1. Definitions

You grant things.

## How to sign

Reply on the PR.
`;

// --- agreementHash ---------------------------------------------------------

test('agreementHash ignores edits outside the agreement section', () => {
  // Fails if the hash covers the whole file: rewording the preamble or the
  // signing instructions would then force every contributor to re-sign.
  const edited = CLA_TEXT.replace('Some preamble.', 'A new preamble.')
    .replace('Reply on the PR.', 'Comment on the PR.');
  assert.equal(cla.agreementHash(edited), cla.agreementHash(CLA_TEXT));
});

test('agreementHash changes when the agreement itself changes', () => {
  // Fails if the hash stops covering the agreement: a revised agreement
  // would silently inherit signatures given to the old terms.
  const edited = CLA_TEXT.replace('You grant things.', 'You grant other things.');
  assert.notEqual(cla.agreementHash(edited), cla.agreementHash(CLA_TEXT));
});

test('agreementHash refuses a CLA without its section headings', () => {
  // Fails if a missing heading falls back to hashing something else, which
  // would record signatures against text nobody identified.
  assert.throws(() => cla.agreementHash('# no sections here\n'), /The agreement/);
});

test('the committed CLA.md has an agreement section and quotes the exact phrase', () => {
  // Fails if CLA.md loses a heading (the workflow would throw on every pull
  // request) or its instructions drift from the phrase the bot accepts.
  const text = require('node:fs').readFileSync(require('node:path').join(__dirname, '../../CLA.md'), 'utf8');
  assert.match(cla.agreementHash(text), /^[0-9a-f]{64}$/);
  assert.ok(text.includes(cla.PHRASE));
});

// --- isSigningComment ------------------------------------------------------

test('isSigningComment accepts the phrase, allowing case, spacing and the final period', () => {
  for (const body of [
    cla.PHRASE,
    `  ${cla.PHRASE}\n`,
    cla.PHRASE.toUpperCase(),
    cla.PHRASE.replace(/\.$/, ''),
    cla.PHRASE.replace(/ /g, '  '),
  ]) {
    assert.equal(cla.isSigningComment(body), true, JSON.stringify(body));
  }
});

test('isSigningComment rejects a quote, a longer comment and other text', () => {
  // Fails if matching is a substring test: quoting the bot's instructions,
  // or "I have read CLA.md and I agree to it. Except section 2", is not an
  // unqualified agreement.
  for (const body of [
    `> ${cla.PHRASE}`,
    `${cla.PHRASE} Except section 2.`,
    'LGTM',
    '',
    null,
  ]) {
    assert.equal(cla.isSigningComment(body), false, JSON.stringify(body));
  }
});

// --- requiredSigners -------------------------------------------------------

function commit(sha, author, message = 'change', email = 'x@example.com') {
  return {
    sha,
    author,
    commit: { author: { name: author ? author.login : 'Someone', email }, message },
  };
}
const alice = { login: 'alice', id: 101, type: 'User' };
const bob = { login: 'bob', id: 202, type: 'User' };
const owner = { login: 'jp2195', id: 24376525, type: 'User' };
const dependabot = { login: 'dependabot[bot]', id: 49699333, type: 'Bot' };

test('requiredSigners lists each commit author once, by account id', () => {
  const { signers, unlinked } = cla.requiredSigners([
    commit('a1', alice), commit('a2', alice), commit('b1', bob),
  ], alice);
  assert.deepEqual(signers.map((s) => s.id).sort(), [101, 202]);
  assert.deepEqual(unlinked, []);
});

test('requiredSigners exempts bots and the copyright holder', () => {
  // Fails if either exemption is dropped: dependabot cannot sign anything,
  // and the holder has nothing to grant to itself.
  const { signers } = cla.requiredSigners([commit('d1', dependabot), commit('o1', owner)], owner);
  assert.deepEqual(signers, []);
});

test('requiredSigners reports a commit whose email matches no account', () => {
  // Fails if an unlinked commit is skipped: its author would contribute
  // without anyone having signed for them.
  const { signers, unlinked } = cla.requiredSigners([
    commit('u1', null, 'change', 'laptop@localhost'),
  ], owner);
  assert.deepEqual(signers, []);
  assert.deepEqual(unlinked, [{ sha: 'u1', name: 'Someone', email: 'laptop@localhost' }]);
});

test('requiredSigners requires co-authors, resolving GitHub noreply addresses', () => {
  // Fails if Co-authored-by trailers are ignored: a co-author wrote part of
  // the change and has signed nothing.
  const { signers, unlinked } = cla.requiredSigners([
    commit('c1', alice,
      'change\n\nCo-authored-by: Bob <202+bob@users.noreply.github.com>\n' +
      'Co-authored-by: Carol <carol@example.com>'),
  ], alice);
  assert.deepEqual(signers.map((s) => s.login).sort(), ['alice', 'bob']);
  assert.deepEqual(unlinked, [{ sha: 'c1', name: 'Carol', email: 'carol@example.com' }]);
});

test('requiredSigners exempts the copyright holder as a co-author', () => {
  const { signers, unlinked } = cla.requiredSigners([
    commit('c2', alice,
      'change\n\nCo-authored-by: josh <24376525+jp2195@users.noreply.github.com>'),
  ], alice);
  assert.deepEqual(signers.map((s) => s.login), ['alice']);
  assert.deepEqual(unlinked, []);
});

const mallory = { login: 'mallory', id: 999, type: 'User' };
const renovate = { login: 'renovate[bot]', id: 29139614, type: 'Bot' };

test('requiredSigners requires whoever opened the pull request', () => {
  // Fails if the opener is not added: commit authorship is matched on an
  // email address anyone can put in a commit, and the opener is the one
  // party GitHub authenticates.
  const { signers } = cla.requiredSigners([commit('a1', alice)], mallory);
  assert.deepEqual(signers.map((s) => s.login).sort(), ['alice', 'mallory']);
});

test('requiredSigners does not let commits attributed to the holder or a bot exempt a third party', () => {
  // Fails if the opener is not required: a fork contributor who authors
  // commits with the holder's noreply address, or a bot's, would have
  // nothing to sign. The opener vouches for those commits instead.
  for (const author of [owner, dependabot]) {
    const { signers, unlinked } = cla.requiredSigners([commit('s1', author)], mallory);
    assert.deepEqual(signers, [{ id: 999, login: 'mallory' }], author.login);
    assert.deepEqual(unlinked, []);
  }
});

test('requiredSigners still exempts pull requests the holder or a bot opened', () => {
  // Fails if either exemption stops applying to the opener: the holder's
  // own pull requests and dependabot's or renovate's would all go red.
  assert.deepEqual(cla.requiredSigners([commit('o1', owner)], owner).signers, []);
  assert.deepEqual(cla.requiredSigners([commit('d1', dependabot)], dependabot).signers, []);
  assert.deepEqual(cla.requiredSigners([commit('r1', renovate)], renovate).signers, []);
});

test('requiredSigners still requires third-party authors in the holder\'s pull request', () => {
  // Fails if an exempt opener exempts everyone: the holder opening a pull
  // request with a contributor's commits does not sign for that contributor.
  const { signers } = cla.requiredSigners([commit('a1', alice), commit('o1', owner)], owner);
  assert.deepEqual(signers.map((s) => s.login), ['alice']);
});

test('requiredSigners refuses to run without the opener', () => {
  // Fails if a missing opener is tolerated: the check would silently fall
  // back to trusting commit emails.
  assert.throws(() => cla.requiredSigners([commit('a1', alice)]), /opener/);
  assert.throws(() => cla.requiredSigners([commit('a1', alice)], {}), /opener/);
});

test('requiredSigners treats a co-author noreply address with an invalid login as unlinked', () => {
  // Fails if the login is not validated: it is text from a commit message,
  // and the bot renders a signer's login as @login, so a trailer could put
  // links, images or arbitrary mentions into the bot's comment.
  const bad = '1+torvalds [click](https://evil.example) ![i](https://evil.example/p.png)@users.noreply.github.com';
  const { signers, unlinked } = cla.requiredSigners([
    commit('c3', alice, `change\n\nCo-authored-by: a <${bad}>\n` +
      'Co-authored-by: b <3+-dash@users.noreply.github.com>\n' +
      `Co-authored-by: c <4+${'x'.repeat(40)}@users.noreply.github.com>\n` +
      'Co-authored-by: d <8+a![i](//e.x)@users.noreply.github.com>'),
  ], alice);
  assert.deepEqual(signers.map((s) => s.login), ['alice']);
  assert.deepEqual(unlinked.map((u) => u.email), [
    bad, '3+-dash@users.noreply.github.com', `4+${'x'.repeat(40)}@users.noreply.github.com`,
    '8+a![i](//e.x)@users.noreply.github.com',
  ]);
});

test('requiredSigners accepts valid co-author logins, including the [bot] form', () => {
  // Fails if validation rejects real logins: hyphens, the 39-character
  // maximum, and a bot's noreply address are all legitimate.
  const long = `a${'b'.repeat(38)}`;
  const { signers, unlinked } = cla.requiredSigners([
    commit('c4', alice, 'change\n\n' +
      'Co-authored-by: A <5+some-one@users.noreply.github.com>\n' +
      `Co-authored-by: B <6+${long}@users.noreply.github.com>\n` +
      'Co-authored-by: C <7+helper[bot]@users.noreply.github.com>'),
  ], alice);
  assert.deepEqual(signers.map((s) => s.login), ['alice', 'some-one', long, 'helper[bot]']);
  assert.deepEqual(unlinked, []);
});

test('requiredSigners exempts the copyright holder by account id, not login', () => {
  // Fails if the exemption keys on the login: a renamed owner account frees
  // "jp2195" for anyone to register, and that stranger would then be exempt.
  const squatter = { login: 'jp2195', id: 999999, type: 'User' };
  const renamedOwner = { login: 'someone-new', id: 24376525, type: 'User' };
  const { signers } = cla.requiredSigners([commit('s1', squatter), commit('o1', renamedOwner)], renamedOwner);
  assert.deepEqual(signers.map((s) => s.id), [999999]);
});

// --- missingSignatures -----------------------------------------------------

test('missingSignatures accepts only signatures of the current agreement', () => {
  // Fails if the hash is not compared: a signature of the draft agreement
  // would cover the revised one.
  const sigs = [{ id: 101, login: 'alice', cla_sha256: 'old' }];
  assert.deepEqual(cla.missingSignatures([alice], sigs, 'new').map((s) => s.id), [101]);
  assert.deepEqual(cla.missingSignatures([alice], sigs, 'old'), []);
});

test('missingSignatures matches by account id, not login', () => {
  // Fails if matching uses the login: logins can be renamed and then
  // claimed by a different person, who would inherit the signature.
  const renamed = [{ id: 101, login: 'alice-old-name', cla_sha256: 'h' }];
  assert.deepEqual(cla.missingSignatures([alice], renamed, 'h'), []);
  const squatter = [{ id: 999, login: 'alice', cla_sha256: 'h' }];
  assert.deepEqual(cla.missingSignatures([alice], squatter, 'h').map((s) => s.id), [101]);
});

// --- run(), end to end against FakeGitHub ----------------------------------

class FakeGitHub {
  constructor({ commits = [], signatures = null, headSha, opener = alice } = {}) {
    this.commits = commits;
    // The pull request's head is its last commit unless a test says
    // otherwise (a push that landed while a run was reading).
    this.opener = opener;
    this.headSha = headSha ?? (commits.length ? commits[commits.length - 1].sha : 'head1');
    this.failOn = null; // name of a REST method that throws, for failure tests
    this.calls = []; // REST calls in order, for tests about ordering
    this.headShas = null;
    this.gets = 0;
    this.files = new Map(); // branch -> { content, sha }
    this.statuses = [];
    this.comments = [];
    this.conflictsToInject = 0;
    this.writes = 0;
    if (signatures) this._store(JSON.stringify(signatures));
    const self = this;
    const err = (status) => Object.assign(new Error(`HTTP ${status}`), { status });
    this.rest = {
      pulls: {
        listCommits: async () => { self.calls.push('listCommits'); return { data: self.commits }; },
        get: async () => {
          self.calls.push('get');
          // headShas, if set, is the head each successive read returns (a
          // push landing mid-run); the last value repeats.
          const sha = self.headShas ? self.headShas[Math.min(self.gets++, self.headShas.length - 1)] : self.headSha;
          return { data: { head: { sha }, user: self.opener } };
        },
      },
      repos: {
        getContent: async ({ ref, path }) => {
          self.calls.push('getContent');
          const f = self.files.get(ref);
          if (!f || path !== cla.SIGNATURES_PATH) throw err(404);
          return { data: { sha: f.sha, content: Buffer.from(f.content).toString('base64'), encoding: 'base64' } };
        },
        createOrUpdateFileContents: async ({ branch, path, content, sha }) => {
          assert.equal(path, cla.SIGNATURES_PATH);
          const f = self.files.get(branch);
          if (!f) throw err(404);
          if (self.conflictsToInject > 0) {
            self.conflictsToInject--;
            self.onConflict?.();
            throw err(409);
          }
          if (f.sha !== sha) throw err(409);
          self._store(Buffer.from(content, 'base64').toString());
          self.writes++;
          return { data: {} };
        },
        createCommitStatus: async (s) => {
          self.calls.push(`status:${s.state}`);
          if (self.failStatusStates?.includes(s.state)) throw err(500);
          self.statuses.push(s);
          return { data: {} };
        },
      },
      git: {
        createBlob: async ({ content }) => ({ data: { sha: 'blob', content } }),
        createTree: async ({ tree }) => ({ data: { sha: 'tree', tree } }),
        createCommit: async ({ parents }) => {
          assert.deepEqual(parents, [], 'the signatures branch must share no history with main');
          return { data: { sha: 'commit' } };
        },
        createRef: async ({ ref }) => {
          assert.equal(ref, `refs/heads/${cla.SIGNATURES_BRANCH}`);
          self._store('[]');
          return { data: {} };
        },
      },
      issues: {
        listComments: async () => ({ data: self.comments }),
        createComment: async ({ body }) => {
          const c = { id: self.comments.length + 1, body, user: { type: 'Bot', login: 'github-actions[bot]' } };
          self.comments.push(c);
          return { data: c };
        },
        updateComment: async ({ comment_id, body }) => {
          self.comments.find((c) => c.id === comment_id).body = body;
          return { data: {} };
        },
      },
    };
  }
  _store(content) {
    this.fileVersion = (this.fileVersion || 0) + 1;
    this.files.set(cla.SIGNATURES_BRANCH, { content, sha: `v${this.fileVersion}` });
  }
  async paginate(fn, params) {
    if (this.failOn === 'paginate') throw Object.assign(new Error('HTTP 502'), { status: 502 });
    return (await fn(params)).data;
  }
  signatures() {
    const f = this.files.get(cla.SIGNATURES_BRANCH);
    return f ? JSON.parse(f.content) : null;
  }
  lastStatus() { return this.statuses[this.statuses.length - 1]; }
  botComments() { return this.comments.filter((c) => c.body.includes(cla.MARKER)); }
}

const repo = { owner: 'jp2195', repo: 'vantage' };
const HASH = cla.agreementHash(CLA_TEXT);
const NOW = () => '2026-10-03T14:22:05Z';

// The head SHA is the FakeGitHub's unless given: run() below fills it in.
function prEvent(opener = alice, headSha) {
  return { eventName: 'pull_request_target', repo,
    payload: { pull_request: { number: 12, head: { sha: headSha }, user: opener } } };
}
function commentEvent(user, body, { onIssue = false } = {}) {
  return { eventName: 'issue_comment', repo,
    payload: {
      issue: { number: 12, ...(onIssue ? {} : { pull_request: { url: 'x' } }) },
      comment: { id: 9001, body, user, html_url: 'https://github.com/jp2195/vantage/pull/12#issuecomment-1' },
    } };
}
async function run(gh, context) {
  const head = context.payload.pull_request?.head;
  if (head && head.sha === undefined) head.sha = gh.headSha;
  await cla.run({ github: gh, context, claText: CLA_TEXT, now: NOW });
}

test('run fails the check and explains how to sign when an author has not signed', async () => {
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  await run(gh, prEvent());
  assert.equal(gh.lastStatus().state, 'failure');
  assert.equal(gh.lastStatus().sha, 'a1');
  assert.equal(gh.lastStatus().context, cla.STATUS_CONTEXT);
  const [c] = gh.botComments();
  assert.match(c.body, /@alice/);
  assert.ok(c.body.includes(cla.PHRASE));
});

test('run passes silently when every author has signed', async () => {
  const gh = new FakeGitHub({ commits: [commit('a1', alice)],
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, prEvent());
  assert.equal(gh.lastStatus().state, 'success');
  assert.deepEqual(gh.botComments(), []);
});

test('run records a signing comment and turns the check green', async () => {
  // Fails if the signature is not persisted, or is stored without the
  // evidence (hash, time, comment link) that makes it worth anything.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  await run(gh, prEvent());
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.deepEqual(gh.signatures(), [{
    id: 101, login: 'alice', signed_at: NOW(), cla_sha256: HASH,
    comment_url: 'https://github.com/jp2195/vantage/pull/12#issuecomment-1',
    comment_id: 9001,
    comment_sha256: require('node:crypto').createHash('sha256').update(cla.PHRASE).digest('hex'),
  }]);
  assert.equal(gh.lastStatus().state, 'success');
  const bots = gh.botComments();
  assert.equal(bots.length, 1, 'the explanation is edited in place, not reposted');
  assert.match(bots[0].body, /signed/i);
  assert.ok(!bots[0].body.includes('@alice'), 'nobody is still asked to sign');
});

test('run creates the signatures branch, with no shared history, on first signature', async () => {
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.equal(gh.signatures().length, 1);
  assert.equal(gh.lastStatus().state, 'success');
});

test('run ignores the phrase from someone who is not an author of the PR', async () => {
  // Fails if any commenter is recorded: a drive-by comment would put a
  // stranger in the signature file with a link to someone else's PR.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  await run(gh, commentEvent(bob, cla.PHRASE));
  assert.deepEqual(gh.signatures(), []);
  assert.equal(gh.lastStatus().state, 'failure');
});

test('run does nothing for an ordinary comment or a comment on an issue', async () => {
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  await run(gh, commentEvent(alice, 'LGTM'));
  await run(gh, commentEvent(alice, cla.PHRASE, { onIssue: true }));
  assert.equal(gh.writes, 0);
  assert.deepEqual(gh.statuses, []);
  assert.deepEqual(gh.comments, []);
});

test('run fails a PR containing an unlinked commit even when everyone else signed', async () => {
  // Fails if unlinked commits only produce a message: the check would go
  // green on a commit nobody can have signed for.
  const gh = new FakeGitHub({
    commits: [commit('a1', alice), commit('u1', null, 'change', 'laptop@localhost')],
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }],
  });
  await run(gh, prEvent());
  assert.equal(gh.lastStatus().state, 'failure');
  const [c] = gh.botComments();
  assert.match(c.body, /u1/);
  assert.match(c.body, /laptop@localhost/);
});

test('run fails closed on a PR at the API 250-commit listing limit', async () => {
  // Fails if the limit is not checked: GitHub lists at most 250 commits for
  // a pull request, so authors past that point would never be asked.
  const commits = Array.from({ length: 250 }, (_, i) => commit(`s${i}`, alice));
  const gh = new FakeGitHub({ commits, signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, prEvent());
  assert.equal(gh.lastStatus().state, 'failure');
  assert.match(gh.botComments()[0].body, /250/);
});

test('run quotes contributor-supplied author names in its comment', async () => {
  // Fails if the name and email are inserted raw: a commit author named
  // "@jp2195" would ping the maintainer, and markup would render.
  const gh = new FakeGitHub({ commits: [
    { sha: 'u2', author: null, commit: { author: { name: '@jp2195 <b>hi</b>', email: 'x@y' }, message: 'm' } },
  ] });
  await run(gh, prEvent());
  assert.ok(gh.botComments()[0].body.includes('`` @jp2195 <b>hi</b> ``'));
});

test('run retries a conflicting write without losing the other signature', async () => {
  // Fails if a 409 is not retried from a fresh read: two contributors
  // signing at once would either error or overwrite each other.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  gh.conflictsToInject = 1;
  gh.onConflict = () => gh._store(JSON.stringify([{ id: 202, login: 'bob', cla_sha256: HASH }]));
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.deepEqual(gh.signatures().map((s) => s.id).sort(), [101, 202]);
});

test('run does not duplicate a signature another run recorded mid-write', async () => {
  // Fails if recordSignature does not re-check after a conflict. A
  // contributor who posts the phrase twice starts two runs, both of which
  // read "unsigned"; the loser of the race must see the winner's entry.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  gh.conflictsToInject = 1;
  gh.onConflict = () => gh._store(JSON.stringify([{ id: 101, login: 'alice', cla_sha256: HASH }]));
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.equal(gh.signatures().length, 1);
});

test('run does not record a second signature of the same agreement', async () => {
  const gh = new FakeGitHub({ commits: [commit('a1', alice)],
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.equal(gh.signatures().length, 1);
  assert.equal(gh.writes, 0);
});

test('run fails a fork pull request whose commits claim the holder\'s or a bot\'s address', async () => {
  // Fails if the opener is not required: GitHub links a commit to the
  // account whose email it carries, which the committer chooses, so these
  // commits look like the holder's or dependabot's but came from mallory.
  for (const author of [owner, dependabot]) {
    const gh = new FakeGitHub({ commits: [commit('s1', author)], signatures: [], opener: mallory });
    await run(gh, prEvent(mallory));
    assert.equal(gh.lastStatus().state, 'failure', author.login);
    assert.match(gh.botComments()[0].body, /@mallory/);
  }
});

test('run fails a pull request whose commits claim a signed contributor\'s address', async () => {
  // Fails if a signature by the commit's apparent author is enough: alice
  // signed, but mallory opened the pull request and has not.
  const gh = new FakeGitHub({ commits: [commit('s2', alice)], opener: mallory,
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, prEvent(mallory));
  assert.equal(gh.lastStatus().state, 'failure');
});

test('run lets the opener sign from a comment, reading the opener from the pull request', async () => {
  // Fails if the comment path does not look up the pull request's opener:
  // the opener would not be among the required signers, so their signing
  // comment would be ignored and the check could never go green.
  const gh = new FakeGitHub({ commits: [commit('s1', owner)], signatures: [], opener: mallory });
  await run(gh, prEvent(mallory));
  await run(gh, commentEvent(mallory, cla.PHRASE));
  assert.deepEqual(gh.signatures().map((s) => s.id), [999]);
  assert.equal(gh.lastStatus().state, 'success');
});

test('run does not take the commenter as the opener', async () => {
  // Fails if the comment path uses the commenter in place of the pull
  // request's opener: the holder commenting on mallory's pull request
  // would then make it look like the holder opened it.
  const gh = new FakeGitHub({ commits: [commit('s1', owner)], signatures: [], opener: mallory });
  await run(gh, commentEvent(owner, cla.PHRASE));
  assert.equal(gh.lastStatus().state, 'failure');
  assert.deepEqual(gh.signatures(), []);
});

test('run passes the holder\'s own pull request and a bot\'s', async () => {
  for (const who of [owner, dependabot, renovate]) {
    const gh = new FakeGitHub({ commits: [commit('x1', who)], signatures: [], opener: who });
    await run(gh, prEvent(who));
    assert.equal(gh.lastStatus().state, 'success', who.login);
    assert.deepEqual(gh.botComments(), []);
  }
});

test('run quotes a co-author login that is not a valid login instead of mentioning it', async () => {
  // Fails if an unvalidated login reaches the comment as @login: the link
  // and image here would render in a comment the repository's bot posted.
  const gh = new FakeGitHub({ opener: mallory,
    signatures: [{ id: 999, login: 'mallory', cla_sha256: HASH }],
    commits: [commit('c5', mallory,
      'x\n\nCo-authored-by: a <1+torvalds [click](https://evil.example) ![i](https://evil.example/p.png)@users.noreply.github.com>')] });
  await run(gh, prEvent(mallory));
  assert.equal(gh.lastStatus().state, 'failure');
  const body = gh.botComments()[0].body;
  assert.ok(!body.includes('@torvalds'), body);
  assert.ok(body.includes('`` 1+torvalds [click](https://evil.example) ![i](https://evil.example/p.png)@users.noreply.github.com ``'), body);
});

// --- the status exists from the start, and never outlives its commit ------

test('run posts pending on the head commit before any other API call', async () => {
  // Fails if pending is not posted, or is posted after the first read: until
  // this status exists, a pull request's own job named `cla` could satisfy
  // the required check, and a failed read would leave it that way.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  await run(gh, prEvent());
  assert.equal(gh.calls[0], 'status:pending');
  assert.deepEqual(gh.statuses[0], {
    ...repo, sha: 'a1', state: 'pending', context: cla.STATUS_CONTEXT,
    description: gh.statuses[0].description,
  });
  assert.equal(gh.lastStatus().state, 'failure');
});

test('run posts error and rethrows when the check fails partway', async () => {
  // Fails if a failure after pending is swallowed (the job would go green)
  // or leaves pending behind instead of a terminal error status.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  gh.failOn = 'paginate';
  await assert.rejects(run(gh, prEvent()), /HTTP 502/);
  assert.deepEqual(gh.statuses.map((s) => [s.sha, s.state]), [['a1', 'pending'], ['a1', 'error']]);
  assert.equal(gh.lastStatus().context, cla.STATUS_CONTEXT);
});

test('run rethrows the original failure when posting error also fails', async () => {
  // Fails if the error-status failure replaces the one that caused it.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  gh.failOn = 'paginate';
  gh.failStatusStates = ['error'];
  await assert.rejects(run(gh, prEvent()), /HTTP 502/);
});

test('run posts error when a signing comment fails partway', async () => {
  // Fails if only the pull_request_target path is guarded.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)] });
  gh.failOn = 'paginate';
  await assert.rejects(run(gh, commentEvent(alice, cla.PHRASE)), /HTTP 502/);
  assert.deepEqual(gh.statuses.map((s) => [s.sha, s.state]), [['a1', 'error']]);
});

test('run posts no verdict when the pull request moved past the event\'s head', async () => {
  // Push A, then force-push B: the run for A reads B's commits. Fails if it
  // posts a verdict on A computed from B, or any verdict or comment at all:
  // the run for B owns B.
  const gh = new FakeGitHub({ commits: [commit('b2', bob)],
    signatures: [{ id: 202, login: 'bob', cla_sha256: HASH }] });
  await run(gh, prEvent(alice, 'a1'));
  assert.deepEqual(gh.statuses.map((s) => [s.sha, s.state]), [['a1', 'pending']]);
  assert.deepEqual(gh.comments, []);
});

test('run follows a signing comment to the head the pull request moved to', async () => {
  // The comment's run reads head a1, then a push moves the pull request to
  // b2. Fails if the run posts nothing (b2 would keep a stale failure: no
  // other event sees this signature), posts a verdict on a1 (a stale SHA),
  // or loses the signature.
  const gh = new FakeGitHub({ commits: [commit('a1', alice), commit('b2', alice)], signatures: [] });
  gh.headShas = ['a1', 'b2'];
  await run(gh, commentEvent(alice, cla.PHRASE));
  assert.equal(gh.signatures().length, 1);
  assert.deepEqual(gh.statuses.map((s) => [s.sha, s.state]), [['b2', 'success']]);
});

test('run fails a signing comment whose pull request head never settles', async () => {
  // Fails if the run follows a moving head forever, or gives up silently.
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  gh.headShas = ['h0', 'h1', 'h2', 'h3', 'h4', 'h5'];
  await assert.rejects(run(gh, commentEvent(alice, cla.PHRASE)), /kept moving/);
  assert.equal(gh.lastStatus().state, 'error');
  assert.notEqual(gh.lastStatus().sha, 'h0', 'the error is posted on the head being evaluated');
});

test('run posts a verdict whatever order the commits are listed in', async () => {
  // GitHub does not document the listing order. Fails if the moved-head
  // check compares the last listed commit with the head: a rebase that
  // reorders commits would leave the check pending forever.
  const gh = new FakeGitHub({ commits: [commit('b2', alice), commit('a1', alice)], headSha: 'b2',
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, prEvent());
  assert.deepEqual(gh.statuses.map((s) => [s.sha, s.state]), [['b2', 'pending'], ['b2', 'success']]);
});

test('run still fails a PR at the listing limit whose head is past the listed commits', async () => {
  // Fails if the stale-head check applies at the limit: GitHub lists only
  // the first 250 commits, so the head is never among them, and the run
  // would post nothing but pending instead of the truncated failure.
  const commits = Array.from({ length: 250 }, (_, i) => commit(`s${i}`, alice));
  const gh = new FakeGitHub({ commits, headSha: 's300',
    signatures: [{ id: 101, login: 'alice', cla_sha256: HASH }] });
  await run(gh, prEvent());
  assert.equal(gh.lastStatus().state, 'failure');
  assert.equal(gh.lastStatus().sha, 's300');
});

test('run records the signing comment\'s id and a hash of its exact text', async () => {
  // Fails if the record keeps only the link: the comment can be edited
  // afterward, and then nothing shows what was agreed to. The hash is of
  // the text as posted, not normalized, so any edit changes it.
  const body = `  ${cla.PHRASE.toUpperCase()}\n`;
  const gh = new FakeGitHub({ commits: [commit('a1', alice)], signatures: [] });
  await run(gh, commentEvent(alice, body));
  const [sig] = gh.signatures();
  assert.equal(sig.comment_id, 9001);
  assert.equal(sig.comment_sha256, require('node:crypto').createHash('sha256').update(body).digest('hex'));
  for (const k of ['id', 'login', 'signed_at', 'cla_sha256', 'comment_url']) assert.ok(k in sig, k);
});
