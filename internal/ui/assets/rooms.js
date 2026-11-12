// Rooms: the list, and one room with its messages (live), open
// requests, claims and members.
import { h, api, errorBox, confirmButton, table, when, rerender, rightsPicker } from './lib.js';

const PAGE = 500; // the server's largest message page

// listMessages returns a room's messages after since, oldest first.
async function listMessages(roomID, since = '') {
  const all = [];
  for (;;) {
    let path = `/v1/rooms/${encodeURIComponent(roomID)}/messages?limit=${PAGE}`;
    if (since) path += '&since=' + encodeURIComponent(since);
    const page = (await api('GET', path)).messages || [];
    all.push(...page);
    if (page.length < PAGE) return all;
    since = page[page.length - 1].ts;
  }
}

const replyTo = m => (m.metadata && Number(m.metadata.reply_to)) || 0;

// unanswered: requests no other key has replied to, as `conclave-mcp status`.
function unanswered(msgs) {
  const author = new Map(msgs.map(m => [m.id, m.author_key_id]));
  const answered = new Set();
  for (const m of msgs) {
    const to = replyTo(m);
    if (to && author.get(to) !== m.author_key_id) answered.add(to);
  }
  const requests = ['question', 'task_assignment', 'handoff', 'alert', 'decision'];
  return msgs.filter(m => requests.includes(m.message_type) && !answered.has(m.id));
}

const extMark = trust => trust === 'external' ? h('span', { class: 'ext' }, 'EXTERNAL') : '';

export async function roomsPage(view) {
  const rooms = (await api('GET', '/v1/rooms')).rooms || [];
  const dayAgo = Date.now() - 24 * 3600 * 1000;
  const rows = await Promise.all(rooms.map(async r => {
    const msgs = await listMessages(r.id);
    const open = unanswered(msgs).filter(m => Date.parse(m.ts) > dayAgo).length;
    return h('tr', {},
      h('td', {}, h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name)),
      h('td', {}, String(open)),
      h('td', {}, msgs.length ? when(msgs[msgs.length - 1].ts) : 'no messages'));
  }));
  const name = h('input', { required: true, maxlength: 128 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Rooms'),
    rooms.length ? table(['Room', 'Open requests (24 h)', 'Last activity'], rows) : h('p', {}, 'No rooms yet.'),
    h('h2', {}, 'Create a room'),
    h('form', {
      onsubmit: async e => {
        e.preventDefault();
        msg.replaceChildren();
        try {
          const room = await api('POST', '/v1/rooms', { name: name.value });
          location.hash = '#/rooms/' + encodeURIComponent(room.id);
        } catch (err) {
          if (err.status === 401) rerender();
          else msg.replaceChildren(errorBox(err));
        }
      },
    }, h('label', {}, 'Name ', name), h('button', {}, 'Create')),
    msg);
}

export async function roomPage(view, id) {
  const base = '/v1/rooms/' + encodeURIComponent(id);
  const [room, members, held, keys] = await Promise.all([
    api('GET', base), api('GET', base + '/members'), api('GET', base + '/claims'), api('GET', '/v1/keys')]);
  const allKeys = keys.keys || [];
  const labels = new Map(allKeys.map(k => [k.id, k.label]));
  const who = m => labels.get(m.author_key_id) || `key ${m.author_key_id}`;
  const msgs = new Map();
  for (const m of await listMessages(id)) msgs.set(m.id, m);

  const list = h('div');
  const requests = h('div');
  const opened = new Set(); // threads the operator expanded

  // rootOf follows reply_to up to the oldest message still present.
  function rootOf(m) {
    for (let i = 0; i < msgs.size; i++) {
      const p = replyTo(m);
      if (!p || !msgs.has(p)) return m.id;
      m = msgs.get(p);
    }
    return m.id;
  }

  function messageEl(m) {
    const ext = m.trust_origin === 'external';
    const meta = Object.assign({}, m.metadata);
    delete meta.reply_to;
    const metaText = Object.keys(meta).length ? JSON.stringify(meta) : '';
    const content = [metaText ? h('pre', { class: 'muted' }, metaText) : '', h('pre', {}, m.content)];
    return h('div', { class: 'message' },
      h('div', { class: 'meta' },
        [`#${m.id}`, when(m.ts), who(m)].map(t => h('span', {}, t)), extMark(m.trust_origin),
        m.message_type && m.message_type !== 'system' ? h('span', {}, `<${m.message_type}>`) : '',
        replyTo(m) ? h('span', {}, `re #${replyTo(m)}`) : '',
        confirmButton('Delete', 'Delete this message?', async () => {
          await api('DELETE', `${base}/messages/${m.id}`);
          msgs.delete(m.id);
          draw();
        })),
      ext
        ? h('div', { class: 'external' },
          h('div', { class: 'fence' }, `--- external content from ${who(m)} ---`),
          content,
          h('div', { class: 'fence' }, '--- end of external content ---'))
        : content);
  }

  function draw() {
    const all = [...msgs.values()].sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : a.id - b.id));
    const replies = new Map();
    for (const m of all) {
      const r = rootOf(m);
      if (r !== m.id) replies.set(r, [...(replies.get(r) || []), m]);
    }
    list.replaceChildren(...all.filter(m => rootOf(m) === m.id).map(m => {
      const el = messageEl(m);
      const thread = replies.get(m.id);
      if (thread) {
        const d = h('details', { open: opened.has(m.id) },
          h('summary', {}, `${thread.length} ${thread.length === 1 ? 'reply' : 'replies'}`),
          thread.map(t => messageEl(t)));
        d.addEventListener('toggle', () => (d.open ? opened.add(m.id) : opened.delete(m.id)));
        el.append(d);
      }
      return el;
    }));
    if (!all.length) list.replaceChildren(h('p', {}, 'No messages.'));
    const open = unanswered(all);
    requests.replaceChildren(...(open.length
      ? open.map(m => h('p', {}, `#${m.id} · ${when(m.ts)} · ${who(m)} <${m.message_type}> `, extMark(m.trust_origin), ' ',
        m.content.length > 120 ? m.content.slice(0, 120) + '…' : m.content))
      : [h('p', {}, 'None.')]));
  }

  draw();
  view.replaceChildren(
    h('h1', {}, room.name),
    room.archived_at ? h('p', { class: 'muted' }, 'Archived ' + when(room.archived_at)) : '',
    h('h2', {}, 'Open requests'), requests,
    h('h2', {}, 'Messages'), list,
    h('h2', {}, 'Active claims'), claimsTable(held.claims || []),
    h('h2', {}, 'Members'), membersSection(base, members.members || [], allKeys),
    room.archived_at ? '' : h('p', {}, confirmButton('Archive room', 'Archive this room? Agents can no longer write to it.', async () => {
      await api('POST', base + '/archive');
      location.hash = '#/';
    })));
  return room.archived_at ? null : follow(base, id, msgs, draw);
}

function claimsTable(claims) {
  if (!claims.length) return h('p', {}, 'None.');
  return table(['Path', 'Held by', 'Until', 'Note'], claims.map(c => h('tr', {},
    h('td', {}, c.path), h('td', {}, c.label, ' ', extMark(c.trust_origin)),
    h('td', {}, when(c.expires_at)), h('td', {}, c.note || ''))));
}

function membersSection(base, members, keys) {
  const rows = members.map(m => {
    const picker = rightsPicker(m.rights || []);
    const msg = h('span');
    return h('tr', {},
      h('td', {}, m.label), h('td', {}, m.trust_origin, ' ', extMark(m.trust_origin)),
      h('td', {}, picker.el, ' ',
        h('button', {
          onclick: async () => {
            msg.replaceChildren();
            try {
              await api('PATCH', `${base}/members/${m.key_id}`, { rights: picker.value() });
              msg.replaceChildren('Saved.');
            } catch (err) {
              if (err.status === 401) rerender();
              else msg.replaceChildren(errorBox(err));
            }
          },
        }, 'Save rights'), ' ', msg),
      h('td', {}, confirmButton('Remove', `Remove ${m.label} from this room?`, async () => {
        await api('DELETE', `${base}/members/${m.key_id}`);
        rerender();
      })));
  });
  const candidates = keys.filter(k => !k.revoked_at && !members.some(m => m.key_id === k.id));
  const key = h('select', {}, candidates.map(k => h('option', { value: k.id }, `${k.label} (${k.trust_origin})`)));
  const picker = rightsPicker();
  const multi = h('input', { type: 'checkbox' });
  const msg = h('div');
  return h('div', {},
    members.length ? table(['Key', 'Trust', 'Rights', ''], rows) : h('p', {}, 'No members.'),
    h('h2', {}, 'Invite a key'),
    candidates.length
      ? h('form', {
        onsubmit: async e => {
          e.preventDefault();
          msg.replaceChildren();
          try {
            await api('POST', `${base}/invite`, { key_id: Number(key.value), rights: picker.value(), allow_multi_room: multi.checked });
            rerender();
          } catch (err) {
            if (err.status === 401) rerender();
            else msg.replaceChildren(errorBox(err));
          }
        },
      }, h('label', {}, 'Key ', key), picker.el,
      h('label', {}, multi, ' even if it is in another room'), h('button', {}, 'Invite'))
      : h('p', {}, 'Every active key is a member. Create one on the Keys page.'),
    msg);
}

// follow reads the room's event stream with fetch (EventSource cannot
// send the X-Conclave-UI header) and reconnects until stopped. On every
// (re)connect it loads what arrived while it was not listening.
function follow(base, id, msgs, draw) {
  const stop = new AbortController();
  const add = list => { for (const m of list) msgs.set(m.id, m); draw(); };
  (async () => {
    while (!stop.signal.aborted) {
      try {
        const res = await fetch(base + '/events?wait=300', { headers: { 'X-Conclave-UI': '1' }, signal: stop.signal });
        if (!res.ok) return; // logged out (401) or archived (410)
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
        let buf = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += value;
          let end;
          while ((end = buf.indexOf('\n\n')) >= 0) {
            const event = buf.slice(0, end);
            buf = buf.slice(end + 2);
            if (event.startsWith(': ok')) { // subscribed: catch up
              const last = [...msgs.values()].reduce((ts, m) => (m.ts > ts ? m.ts : ts), '');
              add(await listMessages(id, last));
            } else if (event.startsWith('event: message.created')) {
              const data = event.split('\n').find(l => l.startsWith('data: '));
              if (data) add([JSON.parse(data.slice(6))]);
            }
          }
        }
      } catch {
        if (stop.signal.aborted) return;
        await new Promise(r => setTimeout(r, 3000));
      }
    }
  })();
  return () => stop.abort();
}
