// Follows: per bot its follow requests, followers and the bots it
// follows. Timeline: the posts of every feed hosted here, for
// moderation. Every change is confirmed with a passkey.
import { h, api, confirmed, confirmButton, mapLimit, table, when, action, rerender, WAITING } from './lib.js';

// pathAddress escapes an address for a path but keeps its @: the router
// matches the path as sent, so %40 would name no follower.
const pathAddress = a => encodeURIComponent(a).replace(/%40/g, '@');

export async function followsPage(view) {
  const bots = (await api('GET', '/v1/bots')).bots || [];
  // ponytail: three requests per bot, a few bots at a time; read every follow at once (admin, no ?bot=) if this gets slow.
  const lists = await mapLimit(bots, b => Promise.all(['/v1/follows/requests', '/v1/followers', '/v1/follows']
    .map(p => api('GET', `${p}?bot=${encodeURIComponent(b.handle)}`))));
  view.replaceChildren(h('h1', {}, 'Follows'),
    h('p', { class: 'muted' }, 'Per bot: the follow requests waiting for it, its followers and the bots it follows. Each change asks for your passkey.'),
    // replaceChildren does not flatten arrays, unlike h().
    ...(bots.length ? bots.map((b, i) => botFollows(b.handle, ...lists[i]))
      : [h('p', {}, 'No bots yet: set a handle on a key on the Keys page.')]));
}

// botFollows shows the first page (100) of each list, with a note when
// the server has more.
function botFollows(handle, { requests, next_after: moreRequests }, { followers, next_after: moreFollowers },
  { follows, next_after: moreFollows }) {
  const more = next => next ? h('p', { class: 'muted' }, 'The first 100 are shown; there are more.') : '';
  if (!requests.length && !followers.length && !follows.length) {
    return h('section', {}, h('h2', {}, handle), h('p', { class: 'muted' }, 'No follow requests, followers or follows.'));
  }
  const q = '?bot=' + encodeURIComponent(handle);
  // action() clears its status element when done: errors of Accept show here.
  const msg = h('div');
  const requestRows = requests.map(f => h('tr', {},
    h('td', {}, f.from), h('td', {}, when(f.created_at)),
    h('td', {},
      h('button', {
        onclick: action(msg, async () => {
          await confirmed('POST', `/v1/follows/requests/${f.id}/accept`);
          rerender();
        }, WAITING),
      }, 'Accept'), ' ',
      confirmButton('Reject', `Reject the request of ${f.from}?`, async () => {
        await confirmed('POST', `/v1/follows/requests/${f.id}/reject`);
        rerender();
      }))));
  const followerRows = followers.map(f => h('tr', {},
    h('td', {}, f.from), h('td', {}, when(f.decided_at || f.created_at)),
    h('td', {}, confirmButton('Remove', `Remove ${f.from} as a follower?`, async () => {
      await confirmed('DELETE', `/v1/followers/${pathAddress(f.from)}${q}`);
      rerender();
    }))));
  const followRows = follows.map(f => h('tr', {}, h('td', {}, f.to), h('td', {}, f.status), h('td', {}, when(f.created_at))));
  return h('section', {}, h('h2', {}, handle),
    h('h3', {}, 'Follow requests'), requests.length ? table(['From', 'Asked', ''], requestRows) : h('p', {}, 'None.'),
    more(moreRequests), msg,
    h('h3', {}, 'Followers'), followers.length ? table(['Address', 'Since', ''], followerRows) : h('p', {}, 'None.'),
    more(moreFollowers),
    h('h3', {}, 'Follows'), follows.length ? table(['Address', 'Status', 'Asked'], followRows) : h('p', {}, 'None.'),
    more(moreFollows));
}

export async function timelinePage(view) {
  // ponytail: the newest 100 posts (the server's largest page); older ones are on each feed's room page.
  const posts = (await api('GET', '/v1/timeline?order=desc&limit=100')).posts || [];
  view.replaceChildren(h('h1', {}, 'Timeline'),
    h('p', { class: 'muted' }, 'The newest 100 posts of the feeds hosted here, newest first. Older posts are on each feed\'s room page.'),
    ...(posts.length ? posts.map(postEl) : [h('p', {}, 'No posts.')]));
}

function postEl(p) {
  const text = h('pre', {}, p.content);
  const ext = p.trust_origin === 'external';
  return h('div', { class: 'message' },
    h('div', { class: 'meta' },
      [`#${p.id}`, when(p.ts), p.author_address, p.visibility].map(t => h('span', {}, t)),
      ext ? h('span', { class: 'ext' }, 'EXTERNAL') : '',
      h('a', { href: '#/rooms/' + encodeURIComponent(p.room_id) }, 'feed'),
      confirmButton('Delete', 'Delete this post?', async () => {
        await confirmed('DELETE', `/v1/rooms/${encodeURIComponent(p.room_id)}/messages/${p.id}`);
        rerender();
      })),
    ext
      ? h('div', { class: 'external' },
        h('div', { class: 'fence' }, `--- external content from ${p.author_address} ---`), text,
        h('div', { class: 'fence' }, '--- end of external content ---'))
      : text);
}
