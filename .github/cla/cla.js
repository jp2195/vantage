// The CLA check behind .github/workflows/cla.yml.
//
// Whoever opened a pull request, and every author of it (commit authors
// and Co-authored-by trailers), must have agreed to CLA.md by commenting
// PHRASE on a pull request. Agreements are recorded in
// SIGNATURES_PATH on SIGNATURES_BRANCH, a branch with no history in common
// with main, so the bot never commits to main.
//
// A signature is keyed by the GitHub account's numeric id, never its login:
// a login can be renamed and later claimed by someone else, who must not
// inherit the signature. It also carries cla_sha256, a hash of the
// agreement section of CLA.md as it stood when the contributor signed.
// Revising the agreement changes the hash, and everyone is asked again,
// because nobody agreed to the new terms. Rewording the rest of CLA.md
// (its preamble, or how to sign) does not.
//
// This runs from pull_request_target, with a token that can write to the
// repository, on pull requests from forks. That is safe only because
// nothing here checks out, installs or executes the pull request's code: it
// reads commit metadata and comments through the API, and nothing else.
// Keep it that way.

const crypto = require('node:crypto');

const PHRASE = 'I have read CLA.md and I agree to it.';
// The copyright holder, by account id (jp2195). An id, not a login: a login can
// be renamed and then registered by someone else, who must not inherit this.
const EXEMPT_IDS = new Set([24376525]);
const SIGNATURES_BRANCH = 'cla-signatures';
const SIGNATURES_PATH = 'signatures.json';
const STATUS_CONTEXT = 'cla';
const MARKER = '<!-- vantage-cla-check -->';
const WRITE_ATTEMPTS = 5;
// GitHub lists at most this many commits for a pull request. At the limit
// the list may be truncated, so authors past it would never be checked.
const MAX_LISTED_COMMITS = 250;

function agreementHash(claText) {
  const start = claText.indexOf('\n## The agreement');
  const end = claText.indexOf('\n## How to sign');
  if (start < 0 || end < 0 || end < start) {
    throw new Error('CLA.md must contain "## The agreement" followed by "## How to sign"');
  }
  const agreement = claText.slice(start, end).trim();
  return crypto.createHash('sha256').update(agreement).digest('hex');
}

function normalize(text) {
  return text.trim().replace(/\s+/g, ' ').replace(/\.$/, '').toLowerCase();
}

function isSigningComment(body) {
  if (typeof body !== 'string') return false;
  return normalize(body) === normalize(PHRASE);
}

function isExempt(user) {
  return user.type === 'Bot' || EXEMPT_IDS.has(user.id);
}

// A noreply address carries the account id and login, so it identifies an
// account without a lookup. Any other address might belong to anyone.
const NOREPLY = /^(\d+)\+([^@]+)@users\.noreply\.github\.com$/i;
const CO_AUTHOR = /^co-authored-by:\s*(.*?)\s*<([^>]+)>\s*$/gim;
// The shape of a GitHub login, or of an app's bot account. A co-author's
// login is text from a commit message, and the bot's comment mentions it
// as @login, so anything else in that position could inject links, images
// or markup into a comment the repository posts.
const LOGIN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$/;

// GitHub links a commit to an account by the email address in the commit,
// which whoever made the commit chooses and nobody verifies. The pull
// request's opener is the one party GitHub authenticates. So the opener
// must always sign unless the opener is exempt, and a commit attributed to
// the copyright holder or a bot only ever excuses itself, never the opener:
// in a pull request a third party opened, such a commit is exactly what a
// forged address looks like, and the opener's signature is what covers it.
function requiredSigners(commits, opener) {
  if (!opener || opener.id === undefined || opener.id === null) {
    throw new Error('requiredSigners needs the pull request opener');
  }
  const signers = new Map();
  const unlinked = [];
  const need = (user) => {
    if (!isExempt(user) && !signers.has(user.id)) {
      signers.set(user.id, { id: user.id, login: user.login });
    }
  };
  need(opener);
  for (const c of commits) {
    if (c.author) {
      need(c.author);
    } else {
      unlinked.push({ sha: c.sha, name: c.commit.author.name, email: c.commit.author.email });
    }
    for (const [, name, email] of c.commit.message.matchAll(CO_AUTHOR)) {
      const m = email.match(NOREPLY);
      if (m && LOGIN.test(m[2])) {
        need({ id: Number(m[1]), login: m[2], type: 'User' });
      } else {
        unlinked.push({ sha: c.sha, name, email });
      }
    }
  }
  return { signers: [...signers.values()], unlinked };
}

function missingSignatures(signers, signatures, hash) {
  const signed = new Set(signatures.filter((s) => s.cla_sha256 === hash).map((s) => s.id));
  return signers.filter((s) => !signed.has(s.id));
}

// --- GitHub I/O --------------------------------------------------------------

async function readSignatures(github, repo) {
  try {
    const { data } = await github.rest.repos.getContent({
      ...repo, path: SIGNATURES_PATH, ref: SIGNATURES_BRANCH,
    });
    const text = Buffer.from(data.content, data.encoding || 'base64').toString();
    return { signatures: JSON.parse(text), sha: data.sha };
  } catch (e) {
    if (e.status === 404) return { signatures: [], sha: null };
    throw e;
  }
}

async function createSignaturesBranch(github, repo) {
  const blob = await github.rest.git.createBlob({ ...repo, content: '[]\n', encoding: 'utf-8' });
  const tree = await github.rest.git.createTree({
    ...repo, tree: [{ path: SIGNATURES_PATH, mode: '100644', type: 'blob', sha: blob.data.sha }],
  });
  const commit = await github.rest.git.createCommit({
    ...repo, message: 'Start the CLA signature record', tree: tree.data.sha, parents: [],
  });
  await github.rest.git.createRef({
    ...repo, ref: `refs/heads/${SIGNATURES_BRANCH}`, sha: commit.data.sha,
  });
}

// Adds a signature, re-reading and retrying on a conflicting write so two
// contributors signing at once cannot overwrite each other.
async function recordSignature(github, repo, signature) {
  for (let attempt = 0; attempt < WRITE_ATTEMPTS; attempt++) {
    let { signatures, sha } = await readSignatures(github, repo);
    if (sha === null) {
      try {
        await createSignaturesBranch(github, repo);
      } catch (e) {
        if (e.status !== 422) throw e; // 422: someone else created it first
      }
      ({ signatures, sha } = await readSignatures(github, repo));
    }
    if (signatures.some((s) => s.id === signature.id && s.cla_sha256 === signature.cla_sha256)) {
      return;
    }
    const next = [...signatures, signature];
    try {
      await github.rest.repos.createOrUpdateFileContents({
        ...repo,
        branch: SIGNATURES_BRANCH,
        path: SIGNATURES_PATH,
        sha,
        message: `Record CLA signature from @${signature.login}`,
        content: Buffer.from(JSON.stringify(next, null, 2) + '\n').toString('base64'),
      });
      return;
    } catch (e) {
      if (e.status !== 409) throw e;
    }
  }
  throw new Error(`could not record the signature after ${WRITE_ATTEMPTS} conflicting writes`);
}

function codeSpan(text) {
  return '`` ' + String(text).replace(/`/g, "'") + ' ``';
}

function renderComment(repo, missing, unlinked, truncated) {
  const claUrl = `https://github.com/${repo.owner}/${repo.repo}/blob/main/CLA.md`;
  if (missing.length === 0 && unlinked.length === 0 && !truncated) {
    return `${MARKER}\nEvery author of this pull request has signed the [CLA](${claUrl}). Thank you.`;
  }
  const lines = [MARKER, 'Thanks for the pull request.', ''];
  if (missing.length > 0) {
    lines.push(
      'vantage needs a Contributor License Agreement from whoever opened this pull request and ' +
      'every author of it before it can be merged. ' +
      `[\`CLA.md\`](${claUrl}) explains why: it is what allows the project to offer a ` +
      'commercial license. You keep your copyright.',
      '',
      `Still to sign: ${missing.map((s) => `@${s.login}`).join(', ')}`,
      '',
      'If you agree, reply to this pull request with exactly:',
      '',
      '```',
      PHRASE,
      '```',
      '',
      'You only need to do this once.',
    );
  }
  if (unlinked.length > 0) {
    if (missing.length > 0) lines.push('');
    lines.push(
      'These commits are from email addresses that do not match a GitHub account, so nobody can ' +
      'sign for them. Add the address to your GitHub account, or amend the commits to use your ' +
      "GitHub noreply address, and push again:",
      '',
      // Author names and emails come from the contributor's commits. Code
      // spans keep them from rendering as mentions, links or HTML.
      ...unlinked.map((u) => `- \`${u.sha.slice(0, 12)}\` ${codeSpan(u.name)} ${codeSpan(u.email)}`),
    );
  }
  if (truncated) {
    lines.push(
      '',
      `This pull request has at least ${MAX_LISTED_COMMITS} commits, the most GitHub will list, so ` +
      'the check cannot see every author. Split it into smaller pull requests, or squash it.',
    );
  }
  return lines.join('\n');
}

async function upsertComment(github, repo, number, body, { create }) {
  const comments = await github.paginate(github.rest.issues.listComments, {
    ...repo, issue_number: number, per_page: 100,
  });
  const existing = comments.find((c) => c.user?.type === 'Bot' && c.body?.includes(MARKER));
  if (existing) {
    if (existing.body !== body) {
      await github.rest.issues.updateComment({ ...repo, comment_id: existing.id, body });
    }
  } else if (create) {
    await github.rest.issues.createComment({ ...repo, issue_number: number, body });
  }
}

// The status is posted on the pull request's head commit. Branch protection
// requires a check named `cla` from no particular source, so a pull request
// whose own workflows define a job named `cla` could satisfy it with a
// check run while this status is absent. The status must therefore exist
// from the moment a run starts: `pending` first, then the verdict, or
// `error` if the run fails before reaching one.
async function postStatus(github, repo, sha, state, description) {
  await github.rest.repos.createCommitStatus({
    ...repo, sha, state, context: STATUS_CONTEXT, description,
  });
}

async function run({ github, context, claText, now = () => new Date().toISOString() }) {
  const repo = { owner: context.repo.owner, repo: context.repo.repo };

  let number;
  let headSha;
  let opener;
  let signer = null;
  if (context.eventName === 'pull_request_target') {
    number = context.payload.pull_request.number;
    headSha = context.payload.pull_request.head.sha;
    opener = context.payload.pull_request.user;
    await postStatus(github, repo, headSha, 'pending', 'Checking that every author has signed the CLA');
  } else if (context.eventName === 'issue_comment') {
    const { issue, comment } = context.payload;
    if (!issue.pull_request || !isSigningComment(comment.body)) return;
    number = issue.number;
    const pr = (await github.rest.pulls.get({ ...repo, pull_number: number })).data;
    headSha = pr.head.sha;
    opener = pr.user;
    signer = comment;
  } else {
    return;
  }

  // check() may move the run to a newer head (see there); an error is
  // posted on whichever head it was evaluating.
  const at = { headSha };
  try {
    await check({ github, repo, claText, now, number, at, opener, signer });
  } catch (e) {
    try {
      await postStatus(github, repo, at.headSha, 'error', 'The CLA check failed to run; see the workflow log');
    } catch {
      // The original failure is the one worth reporting.
    }
    throw e;
  }
}

// How many times a signing comment's run follows a head that keeps moving
// before it gives up and fails.
const HEAD_ATTEMPTS = 3;

async function check({ github, repo, claText, now, number, at, opener, signer }) {
  const hash = agreementHash(claText);

  // The commits listed belong to the head that is current once the listing
  // is done, so the head is read again after it; the order of the listing
  // proves nothing. If the head moved after the event fired (a push, or a
  // force push), there are two cases.
  // - A pull_request_target run posts nothing: the event for the new head
  //   runs its own check, and a verdict here would be about commits that
  //   are no longer the pull request's.
  // - A signing comment's run follows the pull request to its new head and
  //   posts the verdict there. No event for the new head would otherwise
  //   see this signature, and that head could keep a stale failure.
  let commits;
  for (let attempt = 1; ; attempt++) {
    commits = await github.paginate(github.rest.pulls.listCommits, {
      ...repo, pull_number: number, per_page: 100,
    });
    const current = (await github.rest.pulls.get({ ...repo, pull_number: number })).data.head.sha;
    if (current === at.headSha) break;
    if (!signer) return;
    if (attempt >= HEAD_ATTEMPTS) {
      throw new Error(`the pull request's head kept moving (last seen ${current})`);
    }
    at.headSha = current;
  }
  const headSha = at.headSha;
  const truncated = commits.length >= MAX_LISTED_COMMITS;
  const { signers, unlinked } = requiredSigners(commits, opener);

  if (signer) {
    const { signatures } = await readSignatures(github, repo);
    const needed = missingSignatures(signers, signatures, hash);
    if (needed.some((s) => s.id === signer.user.id)) {
      await recordSignature(github, repo, {
        id: signer.user.id,
        login: signer.user.login,
        signed_at: now(),
        cla_sha256: hash,
        comment_url: signer.html_url,
        // A comment can be edited after the fact; its id and a hash of the
        // text as signed pin down what was agreed to.
        comment_id: signer.id,
        comment_sha256: crypto.createHash('sha256').update(signer.body).digest('hex'),
      });
    }
  }

  const { signatures } = await readSignatures(github, repo);
  const missing = missingSignatures(signers, signatures, hash);
  const ok = missing.length === 0 && unlinked.length === 0 && !truncated;

  await postStatus(github, repo, headSha, ok ? 'success' : 'failure', ok
    ? 'Every author has signed the CLA'
    : truncated
      ? `Too many commits (${MAX_LISTED_COMMITS}+) to check every author`
      : `${missing.length} unsigned, ${unlinked.length} unlinked commit author(s)`);
  await upsertComment(github, repo, number, renderComment(repo, missing, unlinked, truncated), { create: !ok });
}

module.exports = {
  PHRASE, SIGNATURES_BRANCH, SIGNATURES_PATH, STATUS_CONTEXT, MARKER,
  agreementHash, isSigningComment, requiredSigners, missingSignatures, run,
};
