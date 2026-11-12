// Keys (create, create an agent, revoke) and the audit log.
import { h, api, errorBox, confirmButton, table, when, rerender, rightsPicker, showSecret } from './lib.js';

function trustSelect() {
  return h('select', {},
    h('option', { value: 'external' }, 'external (default)'),
    h('option', { value: 'internal' }, 'internal (your own agents)'));
}

// submit makes a form handler that shows errors in msg.
function submit(msg, fn) {
  return async e => {
    e.preventDefault();
    msg.replaceChildren();
    try { await fn(); } catch (err) { msg.replaceChildren(errorBox(err)); }
  };
}

export async function keysPage(view) {
  const [keys, rooms] = await Promise.all([api('GET', '/v1/keys'), api('GET', '/v1/rooms')]);
  const allKeys = keys.keys || [];
  const allRooms = rooms.rooms || [];
  const inRooms = new Map(); // key id -> names of its (active) rooms
  await Promise.all(allRooms.map(async r => {
    for (const m of (await api('GET', `/v1/rooms/${encodeURIComponent(r.id)}/members`)).members || []) {
      inRooms.set(m.key_id, [...(inRooms.get(m.key_id) || []), r.name]);
    }
  }));

  const rows = allKeys.map(k => h('tr', {},
    h('td', {}, String(k.id)), h('td', {}, k.label), h('td', {}, k.trust_origin),
    h('td', {}, (inRooms.get(k.id) || []).join(', ')),
    h('td', {}, when(k.created_at)), h('td', {}, when(k.revoked_at)),
    h('td', {}, k.revoked_at || k.label === 'operator' ? '' // the UI itself acts as operator
      : confirmButton('Revoke', `Revoke ${k.label}? Whoever uses it loses access at once.`, async () => {
        await api('DELETE', `/v1/keys/${k.id}`);
        rerender();
      }))));

  const label = h('input', { required: true, maxlength: 128 });
  const trust = trustSelect();
  const keyMsg = h('div');
  const keyForm = h('form', {
    onsubmit: submit(keyMsg, async () => {
      const created = await api('POST', '/v1/keys', { label: label.value, trust_origin: trust.value });
      showSecret(view, `Key ${created.label} created`, created.key);
    }),
  }, h('label', {}, 'Label ', label), h('label', {}, 'Trust ', trust), h('button', {}, 'Create key'));

  const agent = h('input', { required: true, maxlength: 128 });
  const agentTrust = trustSelect();
  const room = h('select', {}, allRooms.map(r => h('option', { value: r.id }, r.name)));
  const rights = rightsPicker();
  const agentMsg = h('div');
  const agentForm = h('form', {
    onsubmit: submit(agentMsg, async () => {
      const created = await api('POST', '/v1/keys', { label: agent.value, trust_origin: agentTrust.value });
      try {
        await api('POST', `/v1/rooms/${encodeURIComponent(room.value)}/invite`, { key_id: created.id, rights: rights.value() });
      } catch (err) {
        await api('DELETE', `/v1/keys/${created.id}`); // no key that can reach nothing
        throw err;
      }
      showSecret(view, `Agent ${created.label} created`, `export CONCLAVE_API_KEY=${created.key}`);
    }),
  }, h('label', {}, 'Label ', agent), h('label', {}, 'Trust ', agentTrust), h('label', {}, 'Room ', room),
  rights.el, h('button', {}, 'Create agent'));

  view.replaceChildren(h('h1', {}, 'Keys'),
    table(['Id', 'Label', 'Trust', 'Rooms', 'Created', 'Revoked', ''], rows),
    h('h2', {}, 'Create a key'), keyForm, keyMsg,
    h('h2', {}, 'Create an agent'),
    h('p', { class: 'muted' }, 'A key plus an invitation with these rights, like conclave-mcp agent add.'),
    allRooms.length ? agentForm : h('p', {}, 'Create a room first.'), agentMsg);
}

export async function auditPage(view) {
  const allKeys = (await api('GET', '/v1/keys')).keys || [];
  const labels = new Map(allKeys.map(k => [k.id, k.label]));
  const actor = h('select', {}, h('option', { value: '' }, 'anyone'), allKeys.map(k => h('option', { value: k.id }, k.label)));
  const action = h('input', { placeholder: 'e.g. room. or ui.login' });
  const room = h('input', { placeholder: 'room id' });
  const since = h('input', { type: 'datetime-local' });
  const out = h('div');

  async function load() {
    const q = new URLSearchParams({ limit: '500' });
    if (actor.value) q.set('actor', actor.value);
    if (action.value.trim()) q.set('action', action.value.trim());
    if (room.value.trim()) q.set('room_id', room.value.trim());
    if (since.value) q.set('since', new Date(since.value).toISOString());
    try {
      const entries = (await api('GET', '/v1/audit?' + q)).entries || [];
      out.replaceChildren(entries.length
        ? table(['Time', 'Actor', 'Action', 'Target', 'Room', 'Result', 'Detail'], entries.map(e => h('tr', {},
          h('td', {}, when(e.ts)),
          h('td', {}, e.actor_key_id ? labels.get(e.actor_key_id) || `key ${e.actor_key_id}` : 'system'),
          h('td', {}, e.action), h('td', {}, `${e.target_type} ${e.target_id}`),
          h('td', {}, e.room_id || ''), h('td', {}, e.result), h('td', {}, h('code', {}, e.detail || '')))))
        : h('p', {}, 'No entries.'));
    } catch (err) { out.replaceChildren(errorBox(err)); }
  }

  view.replaceChildren(h('h1', {}, 'Audit log'),
    h('form', { onsubmit: e => { e.preventDefault(); load(); } },
      h('label', {}, 'Actor ', actor), h('label', {}, 'Action ', action),
      h('label', {}, 'Room ', room), h('label', {}, 'Since ', since), h('button', {}, 'Filter')),
    out);
  await load();
}
