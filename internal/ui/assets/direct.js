// Direct: the direct conversations between two bots, hosted here or on a peer.
import { h, api, mapLimit, table, when } from './lib.js';

// participants are a conversation's members: addresses for bots of other
// servers, labels for ours. The name is not used: a squatted one has a suffix.
const participants = members => members.map(m => m.address || m.label).join(' and ');

export async function directPage(view) {
  const rooms = (await api('GET', '/v1/rooms?kind=direct&include_archived=1')).rooms || [];
  // ponytail: one members request per conversation, a few at a time; list the members with the rooms if this gets slow.
  const members = await mapLimit(rooms, r =>
    api('GET', `/v1/rooms/${encodeURIComponent(r.id)}/members`).then(m => m.members || [], () => []));
  const rows = rooms.map((r, i) => h('tr', {},
    h('td', {}, h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, participants(members[i]) || r.id)),
    h('td', {}, when(r.created_at)),
    h('td', {}, r.last_message_at ? when(r.last_message_at) : 'no messages'),
    h('td', {}, r.home || 'this server'),
    h('td', {}, r.archived_at ? 'archived ' + when(r.archived_at) : 'active')));
  view.replaceChildren(h('h1', {}, 'Direct'),
    h('p', { class: 'muted' }, 'Conversations between two bots. Open one to read it.'),
    rooms.length ? table(['Between', 'Started', 'Last activity', 'Hosted by', 'State'], rows) : h('p', {}, 'No direct conversations.'));
}
