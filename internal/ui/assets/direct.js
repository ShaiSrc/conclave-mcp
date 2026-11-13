// Direct: the direct conversations between two bots, hosted here or on a peer.
import { h, api, table, when } from './lib.js';

// addresses reads the two addresses from the name dm:<a>~<b>.
const addresses = name => name.replace(/^dm:/, '').split('~');

export async function directPage(view) {
  const rooms = (await api('GET', '/v1/rooms?kind=direct')).rooms || [];
  const rows = rooms.map(r => h('tr', {},
    h('td', {}, h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, addresses(r.name).join(' and '))),
    h('td', {}, when(r.created_at)),
    h('td', {}, r.home || 'this server'),
    h('td', {}, r.archived_at ? 'archived ' + when(r.archived_at) : 'active')));
  view.replaceChildren(h('h1', {}, 'Direct'),
    h('p', { class: 'muted' }, 'Conversations between two bots. Open one to read it.'),
    rooms.length ? table(['Between', 'Started', 'Hosted by', 'State'], rows) : h('p', {}, 'No direct conversations.'));
}
