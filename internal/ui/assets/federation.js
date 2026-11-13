// Federation: this server's identity, its peers and adding a peer. Every
// change (add, block, remove, ping) is confirmed with a passkey: a UI
// session alone can only read, and a ping is a POST.
import { h, api, confirmed, WAITING, errorBox, confirmButton, table, when, action, rerender } from './lib.js';

const peerPath = (domain, rest = '') => `/v1/federation/peers/${encodeURIComponent(domain)}${rest}`;

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

function peerRow(p) {
  const out = h('span');
  const blocked = p.status === 'blocked';
  return h('tr', {},
    h('td', {}, p.domain), h('td', {}, p.status),
    h('td', {}, h('code', {}, p.fingerprint || '')),
    h('td', {}, p.note || ''), h('td', {}, when(p.last_seen_at)),
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
          const r = await confirmed('POST', peerPath(p.domain, '/ping'));
          out.replaceChildren(`${r.rtt_ms} ms`);
        }, WAITING),
      }, 'Ping'), ' ', out));
}

export async function federationPage(view) {
  const [status, list] = await Promise.all([api('GET', '/v1/federation'), api('GET', '/v1/federation/peers')]);
  const peers = list.peers || [];

  const domain = h('input', { required: true, maxlength: 253, placeholder: 'conclave.example.org' });
  const note = h('input', { maxlength: 200 });
  const msg = h('div');
  const found = h('div'); // step 2: shown after a lookup
  const lookup = h('button', {
    onclick: action(msg, async () => {
      found.replaceChildren();
      const r = await api('GET', '/v1/federation/lookup?domain=' + encodeURIComponent(domain.value.trim()));
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
  domain.addEventListener('input', () => found.replaceChildren());

  view.replaceChildren(h('h1', {}, 'Federation'),
    h('p', {}, `Domain ${status.domain}, mode ${status.mode}, key ${status.key_id}`),
    fingerprintBlock(status.fingerprint),
    h('h2', {}, 'Peers'),
    peers.length ? table(['Domain', 'Status', 'Fingerprint', 'Note', 'Last contact', ''], peers.map(peerRow)) : h('p', {}, 'No peers.'),
    h('h2', {}, 'Add a peer'),
    h('label', {}, 'Domain ', domain), ' ', lookup, msg, found);
}
