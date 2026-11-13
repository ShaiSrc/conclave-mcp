// Bots: the local bots with their profiles (edit, visibility) and the
// directory of local and peer bots. Saving is a confirmed change.
import { h, api, confirmed, WAITING, errorBox, table, action, rerender, when } from './lib.js';

const VISIBILITY = ['private', 'peers', 'public'];

// domainOf is this server's federation domain, '' with federation off.
async function domainOf() {
  try {
    return (await api('GET', '/v1/federation')).domain || '';
  } catch (err) {
    if (err.status === 401) throw err;
    return '';
  }
}

// publishNote says in words who will see the profile once saved.
function publishNote(vis, domain, handle) {
  if (vis === 'public') {
    return domain
      ? `Visibility public: anyone on the web can see this profile at https://${domain}/@${handle}`
      : 'Visibility public: anyone who can reach this server can see this profile.';
  }
  if (vis === 'peers') return 'Visibility peers: every key on this server and the servers you allowed as peers can see this profile.';
  return 'Visibility private: only admin keys and the bot itself can see this profile.';
}

function skillRow(s, rows) {
  const id = h('input', { value: s.id, required: true, maxlength: 32, pattern: '[a-z0-9_.\\-]{1,32}', placeholder: 'id', 'aria-label': 'Skill id' });
  const name = h('input', { value: s.name, required: true, maxlength: 64, placeholder: 'name', 'aria-label': 'Skill name' });
  const desc = h('input', { value: s.description, maxlength: 300, placeholder: 'description', 'aria-label': 'Skill description' });
  const row = h('div', { class: 'skill' }, id, ' ', name, ' ', desc, ' ',
    h('button', { type: 'button', onclick: () => { rows.splice(rows.indexOf(row), 1); row.remove(); } }, 'Remove'));
  row.value = () => ({ id: id.value.trim(), name: name.value.trim(), description: desc.value.trim() });
  return row;
}

function botEditor(p, domain) {
  const name = h('input', { value: p.display_name, maxlength: 64 });
  const desc = h('textarea', { rows: 3, maxlength: 1000 }, p.description);
  const vis = h('select', {}, VISIBILITY.map(v => h('option', { value: v, selected: v === p.visibility }, v)));
  const rows = [];
  const list = h('div');
  const add = s => { const r = skillRow(s, rows); rows.push(r); list.append(r); };
  (p.skills || []).forEach(add);
  const addBtn = h('button', { type: 'button', onclick: () => { if (rows.length < 20) add({ id: '', name: '', description: '' }); } }, 'Add skill');
  const note = h('p', { class: 'muted' }, publishNote(p.visibility, domain, p.handle));
  vis.addEventListener('change', () => note.replaceChildren(publishNote(vis.value, domain, p.handle)));
  const msg = h('div');
  // Only changed fields are sent, so a save cannot undo another
  // writer's change to a field this form did not touch.
  const loadedSkills = JSON.stringify((p.skills || []).map(s => ({ id: s.id, name: s.name, description: s.description })));
  const save = h('button', {
    onclick: action(msg, async () => {
      const patch = {};
      if (name.value !== p.display_name) patch.display_name = name.value;
      if (desc.value !== p.description) patch.description = desc.value;
      const skills = rows.map(r => r.value());
      if (JSON.stringify(skills) !== loadedSkills) patch.skills = skills;
      if (vis.value !== p.visibility) patch.visibility = vis.value;
      if (!Object.keys(patch).length) throw new Error('Nothing to save: no field changed.');
      await confirmed('PATCH', `/v1/bots/${encodeURIComponent(p.handle)}/profile`, patch);
      rerender();
    }, WAITING),
  }, 'Save');
  // a button outside a form would not check maxlength or pattern: use a form
  const form = h('form', { onsubmit: e => { e.preventDefault(); save.click(); } },
    h('label', {}, 'Display name ', name), h('label', {}, 'Description ', desc),
    h('div', {}, h('strong', {}, 'Skills (at most 20)'), list, addBtn),
    h('label', {}, 'Visibility ', vis), note, save, msg);
  return h('details', {}, h('summary', {}, 'Edit'), form);
}

export async function botsPage(view) {
  const [list, domain] = await Promise.all([api('GET', '/v1/bots'), domainOf()]);
  const all = list.bots || [];
  const rows = all.map(p => h('tr', {},
    h('td', {}, p.handle), h('td', {}, domain ? `${p.handle}@${domain}` : p.handle),
    h('td', {}, p.display_name), h('td', {}, p.visibility),
    h('td', {}, p.description.length > 80 ? p.description.slice(0, 80) + '…' : p.description),
    h('td', {}, p.updated_at ? when(p.updated_at) : '—'),
    h('td', {}, botEditor(p, domain))));
  // action() clears its status element when done, so the result goes
  // into its own element.
  const dir = h('div');
  const dirStatus = h('span', { class: 'muted' });
  const show = h('button', {
    onclick: action(dirStatus, async () => {
      const entries = (await api('GET', '/v1/directory')).bots || [];
      dir.replaceChildren(entries.length
        ? table(['Address', 'Where', 'Display name', 'Description'], entries.map(e => h('tr', {},
          h('td', {}, e.address), h('td', {}, e.remote ? `remote: ${e.address.split('@')[1] || ''}` : 'this server'),
          h('td', {}, e.display_name), h('td', {}, e.description))))
        : h('p', {}, 'No bots.'));
    }),
  }, 'Show directory');
  view.replaceChildren(h('h1', {}, 'Bots'),
    all.length ? table(['Handle', 'Address', 'Display name', 'Visibility', 'Description', 'Updated', ''], rows)
      : h('p', {}, 'No bots yet: set a handle on a key on the Keys page.'),
    h('h2', {}, 'Directory'),
    h('p', { class: 'muted' }, 'Local bots you may see and the cached bots of peers.'), show, dirStatus, dir);
}
