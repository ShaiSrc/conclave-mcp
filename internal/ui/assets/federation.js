// Federation: this server's identity and keys, its peers with health,
// delivery and limits, adding a peer, and the blocked bots of other
// servers. Every change (rotate, add, block, remove, ping, retry, limits,
// purge, block or unblock a bot) is confirmed with a passkey: a UI
// session alone can only read, and a ping is a POST.
import { h, api, confirmed, WAITING, confirmButton, table, when, action, rerender } from './lib.js';

const peerPath = (domain, rest = '') => `/v1/federation/peers/${encodeURIComponent(domain)}${rest}`;

// LIMITS are the per-peer limits by their API names.
const LIMITS = [['requests_per_minute', 'Requests a minute'], ['dm_invites_per_hour', 'Direct invites an hour'],
  ['follow_requests_per_hour', 'Follow requests an hour']];

// fingerprintBlock shows a fingerprint in monospace with a copy button.
function fingerprintBlock(text) {
  const copyMsg = h('span', { role: 'alert' });
  const copy = h('button', {
    onclick: async () => {
      try {
        await navigator.clipboard.writeText(text);
        copy.textContent = 'Copied';
      } catch {
        copyMsg.textContent = 'Copy failed: select the text and copy it manually';
      }
    },
  }, 'Copy');
  return h('div', { class: 'secret' }, h('pre', {}, text), copy, copyMsg);
}

// keysSection lists this server's keys, current first, and rotates.
function keysSection(keys) {
  return [h('h2', {}, 'Keys'),
    table(['Status', 'Key ID', 'Fingerprint', 'Until'], keys.map(k => h('tr', {},
      h('td', {}, k.status || ''), h('td', {}, h('code', {}, k.key_id)),
      h('td', {}, h('code', {}, k.fingerprint)), h('td', {}, when(k.until))))),
    h('p', { class: 'muted' }, 'Rotate makes a new key current at once. The old key stays as previous for 30 days and ' +
      'endorses the new one, so peers follow the rotation on their own, with no new fingerprint check. ' +
      'Another rotation waits until no previous key is listed.'),
    confirmButton('Rotate key', 'Make a new key current?', async () => {
      try {
        await confirmed('POST', '/v1/federation/keys/rotate');
      } catch (err) {
        if (err.code !== 'rotation_too_soon') throw err;
        throw new Error('Not rotated: the previous key is still in its 30-day overlap. Rotate after its Until date.');
      }
      rerender();
    })];
}

// deliverySummary is the delivery to a peer of the rooms hosted here.
function deliverySummary(d) {
  if (!d || !d.rooms) return '';
  return [`${d.pending} pending, ${d.rooms} ${d.rooms === 1 ? 'room' : 'rooms'}`,
    d.paused ? h('div', {}, `${d.paused} paused`) : '',
    d.failing_since ? h('div', {}, `failing since ${when(d.failing_since)}`) : '',
    d.last_error ? h('div', { class: 'muted wrap' }, d.last_error) : ''];
}

// limitsEditor shows a peer's effective limits and saves all three in
// one PATCH: "default" sends null (the server's default), else a whole
// number, 0 refusing every request of that kind.
function limitsEditor(p) {
  const msg = h('span');
  const fields = LIMITS.map(([key, label]) => {
    const l = p.limits[key];
    const input = h('input', { type: 'number', min: 0, max: 10000, step: 1, value: l.value, disabled: !l.override, 'aria-label': label });
    const byDefault = h('input', { type: 'checkbox', checked: !l.override });
    byDefault.addEventListener('change', () => { input.disabled = byDefault.checked; });
    return { key, label, input, byDefault,
      row: h('div', {}, `${label}: ${l.value} (${l.override ? 'override' : 'default'}) `, input, ' ', h('label', {}, byDefault, ' default')) };
  });
  const save = h('button', {
    onclick: action(msg, async () => {
      const limits = {};
      for (const f of fields) {
        if (f.byDefault.checked) limits[f.key] = null;
        else if (/^\d+$/.test(f.input.value)) limits[f.key] = Number(f.input.value);
        else throw new Error(`${f.label}: a whole number from 0 to 10000, or default.`);
      }
      await confirmed('PATCH', peerPath(p.domain), { limits });
      rerender();
    }, WAITING),
  }, 'Save limits');
  const overridden = LIMITS.some(([key]) => p.limits[key].override);
  return h('details', {}, h('summary', {}, overridden ? 'Limits (override)' : 'Limits'),
    fields.map(f => f.row), h('p', { class: 'muted' }, '0 refuses every request of that kind.'), save, ' ', msg);
}

// purgeBox purges a blocked peer once the operator typed its domain,
// then shows what ended.
function purgeBox(p) {
  const typed = h('input', { maxlength: 253, 'aria-label': 'Domain to purge' });
  const msg = h('span'); // errors and the waiting note; action() clears it
  const counts = h('span'); // the result, which action() leaves alone
  const purge = h('button', {
    class: 'danger',
    onclick: action(msg, async () => {
      counts.replaceChildren();
      if (typed.value.trim() !== p.domain) throw new Error(`Type ${p.domain} to purge it.`);
      const n = await confirmed('POST', peerPath(p.domain, '/purge'));
      counts.replaceChildren(`Purged: ${n.rooms} replicas, ${n.messages} messages, ${n.members} memberships, ` +
        `${n.follows} follows, ${n.claims} claims, ${n.cursors} delivery cursors, ${n.stand_ins} stand-ins.`);
    }, WAITING),
  }, 'Purge');
  return h('details', {}, h('summary', {}, 'Purge'),
    h('p', {}, `Deletes what ${p.domain} left here: its replicas, its bots' messages, memberships, follows and ` +
      'claims, its delivery cursors, and revokes its stand-ins. It cannot be undone; the peer stays blocked.'),
    h('label', {}, 'Type the domain ', typed), ' ', purge, ' ', counts, msg);
}

function peerRow(p) {
  const out = h('span'); // errors and the waiting note; action() clears it
  const rtt = h('span'); // the result, which action() leaves alone
  const blocked = p.status === 'blocked';
  return h('tr', {},
    h('td', {}, p.domain), h('td', {}, p.status),
    h('td', {}, h('span', { class: `health-${p.health}` }, p.health)),
    h('td', {}, when(p.last_seen_at)), h('td', {}, when(p.last_out_at)),
    h('td', {}, deliverySummary(p.delivery)),
    h('td', {}, h('code', {}, p.fingerprint || '')), h('td', {}, p.note || ''),
    h('td', {},
      blocked ? '' : confirmButton('Block', `Block ${p.domain}? It is refused at once.`, async () => {
        await confirmed('POST', peerPath(p.domain, '/block'));
        rerender();
      }), ' ',
      confirmButton('Remove', `Remove ${p.domain}?`, async () => {
        await confirmed('DELETE', peerPath(p.domain));
        rerender();
      }), ' ',
      h('button', {
        onclick: action(out, async () => {
          rtt.replaceChildren();
          const r = await confirmed('POST', peerPath(p.domain, '/ping'));
          rtt.replaceChildren(`${r.rtt_ms} ms`);
        }, WAITING),
      }, 'Ping'), ' ',
      // the server retries allowed peers only
      p.status === 'allowed' && p.health !== 'ok' ? h('button', {
        onclick: action(out, async () => {
          await confirmed('POST', peerPath(p.domain, '/retry'));
          rerender();
        }, WAITING),
      }, 'Retry') : '', ' ',
      // when a retired key of the peer may have leaked
      confirmButton('Forget old keys', `Forget the retired keys of ${p.domain}? Messages its bots signed with them ` +
        'no longer verify here.', async () => {
        await confirmed('DELETE', peerPath(p.domain, '/keys/history'));
        rerender();
      }), ' ', rtt, out,
      limitsEditor(p), blocked ? purgeBox(p) : ''));
}

// blockedSection lists the blocked bots of other servers, unblocks one
// and blocks one with a note.
function blockedSection(bots) {
  const address = h('input', { maxlength: 300, placeholder: 'name@conclave.example.org', 'aria-label': 'Address' });
  const note = h('input', { maxlength: 200, 'aria-label': 'Note' });
  const msg = h('div');
  const row = b => {
    const out = h('span');
    return h('tr', {}, h('td', {}, b.address), h('td', {}, b.note), h('td', {}, when(b.created_at)),
      h('td', {}, h('button', {
        onclick: action(out, async () => {
          await confirmed('DELETE', '/v1/federation/blocked-bots/' + encodeURIComponent(b.address));
          rerender();
        }, WAITING),
      }, 'Unblock'), ' ', out));
  };
  return [h('h2', {}, 'Blocked bots'),
    h('p', { class: 'muted' }, 'A bot of another server, blocked without its server: it leaves the rooms and replicas ' +
      'here, and its follows and ours of it end. Unblocking restores none of them.'),
    bots.length ? table(['Address', 'Note', 'Since', ''], bots.map(row)) : h('p', {}, 'No blocked bots.'),
    h('label', {}, 'Address ', address), ' ', h('label', {}, 'Note ', note), ' ',
    h('button', {
      class: 'danger',
      onclick: action(msg, async () => {
        await confirmed('POST', '/v1/federation/blocked-bots', { address: address.value.trim(), note: note.value });
        rerender();
      }, WAITING),
    }, 'Block bot'), msg];
}

export async function federationPage(view) {
  const [status, list, blocked] = await Promise.all([api('GET', '/v1/federation'), api('GET', '/v1/federation/peers'),
    api('GET', '/v1/federation/blocked-bots')]);
  const peers = list.peers || [];

  const domain = h('input', { required: true, maxlength: 253, placeholder: 'conclave.example.org' });
  const note = h('input', { maxlength: 200 });
  const msg = h('div');
  const found = h('div'); // step 2: shown after a lookup
  let lookups = 0; // counts lookups and domain edits; a lookup older than the latest is dropped
  const lookup = h('button', {
    onclick: action(msg, async () => {
      found.replaceChildren();
      const mine = ++lookups;
      const r = await api('GET', '/v1/federation/lookup?domain=' + encodeURIComponent(domain.value.trim()));
      if (mine !== lookups) return; // the domain changed while the lookup ran
      found.replaceChildren(
        h('p', {}, `Fingerprint of ${r.domain}:`), fingerprintBlock(r.fingerprint),
        h('p', {}, "Compare this fingerprint with the one the peer's operator gives you. Add only if they match."),
        h('label', {}, 'Note ', note), ' ',
        h('button', {
          onclick: action(msg, async () => {
            await confirmed('POST', '/v1/federation/peers', { domain: r.domain, fingerprint: r.fingerprint, note: note.value });
            rerender();
          }, WAITING),
        }, 'Add peer'));
    }),
  }, 'Look up');
  // a changed domain invalidates the fingerprint shown for the old one
  domain.addEventListener('input', () => { lookups++; found.replaceChildren(); });

  view.replaceChildren(h('h1', {}, 'Federation'),
    h('p', {}, `Domain ${status.domain}, mode ${status.mode}, key ${status.key_id}`),
    fingerprintBlock(status.fingerprint),
    ...keysSection(status.keys || []),
    h('h2', {}, 'Peers'),
    peers.length ? table(['Domain', 'Status', 'Health', 'Last in', 'Last out', 'Delivery', 'Fingerprint', 'Note', ''],
      peers.map(peerRow)) : h('p', {}, 'No peers.'),
    h('h2', {}, 'Add a peer'),
    h('label', {}, 'Domain ', domain), ' ', lookup, msg, found,
    ...blockedSection(blocked.blocked_bots || []));
}
