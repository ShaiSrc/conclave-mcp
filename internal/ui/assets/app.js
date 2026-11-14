// The page: routing, the header, passkey login and setup, passkeys.
import { h, api, confirmed, confirmNow, oneAtATime, WAITING, setSession, errorBox, confirmButton, table, when, action } from './lib.js';
import { roomsPage, roomPage } from './rooms.js';
import { keysPage, auditPage } from './keys.js';
import { federationPage } from './federation.js';
import { botsPage } from './bots.js';
import { directPage } from './direct.js';
import { followsPage, timelinePage } from './follow.js';

const main = document.getElementById('app');
const nav = document.getElementById('nav');

// registerPasskey runs a registration ceremony: with a setup token for
// the first passkey (which logs this tab in), else as the logged-in
// operator, confirmed first with a passkey already registered (a session
// alone cannot add one).
const registerPasskey = (name, setupToken = '') => oneAtATime(async () => {
  let options;
  try {
    options = setupToken
      ? await api('POST', '/ui/auth/register/begin', { name, setup_token: setupToken })
      : await confirmNow('POST', '/ui/auth/register/begin', { name });
  } catch (err) {
    // an expired or used token is of no use: drop it from the address bar
    if (setupToken && err.status >= 400 && err.status < 500) history.replaceState(null, '', '#/');
    throw err;
  }
  const cred = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options) });
  const res = await api('POST', '/ui/auth/register/finish', cred.toJSON());
  if (res.session) setSession(res.session);
});

const login = () => oneAtATime(async () => {
  const options = await api('POST', '/ui/auth/login/begin');
  const cred = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options) });
  setSession((await api('POST', '/ui/auth/login/finish', cred.toJSON())).session);
});

// logout ends the session on the server; the tab forgets its token
// even if that fails.
async function logout() {
  try { await api('POST', '/ui/auth/logout'); } finally { setSession(''); }
}

function loginPage(view, state) {
  if (state.setup_needed) {
    view.replaceChildren(h('h1', {}, 'Set up the web UI'),
      h('p', {}, 'No passkey is registered yet. With the admin key, run'),
      h('pre', {}, 'conclave-mcp ui setup'),
      h('p', {}, 'and open the URL it prints in this browser.'));
    return;
  }
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Log in'),
    h('button', { onclick: action(msg, async () => { await login(); await render(); }, WAITING, false) }, 'Log in with a passkey'),
    msg);
}

function setupPage(view, token) {
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Register your passkey'),
    h('p', {}, 'The passkey (Touch ID or another authenticator on this device) is how you log in to this server from now on.'),
    h('label', {}, 'Name ', name), ' ',
    h('button', {
      onclick: action(msg, async () => {
        await registerPasskey(name.value, token);
        history.replaceState(null, '', '#/'); // drop the used token from the address bar
        await render();
      }, WAITING, false),
    }, 'Register passkey'),
    msg);
}

async function passkeysPage(view) {
  const { credentials } = await api('GET', '/ui/auth/credentials');
  const name = h('input', { value: 'passkey', maxlength: 64 });
  const msg = h('div');
  view.replaceChildren(h('h1', {}, 'Passkeys'),
    table(['Name', 'Added', 'Last used', ''], credentials.map(c => h('tr', {},
      h('td', {}, c.name), h('td', {}, when(c.created_at)), h('td', {}, when(c.last_used_at)),
      h('td', {}, credentials.length > 1
        ? confirmButton('Remove', 'Remove this passkey?', async () => {
          await confirmed('DELETE', '/ui/auth/credentials/' + encodeURIComponent(c.id));
          await render();
        })
        : h('span', { class: 'muted' }, 'the last passkey cannot be removed'))))),
    h('h2', {}, 'Add a passkey'),
    h('label', {}, 'Name ', name), ' ',
    h('button', { onclick: action(msg, async () => { await registerPasskey(name.value); await render(); }, WAITING) }, 'Add passkey'),
    msg);
}

// header returns the navigation: pages, the rooms, logout.
async function header() {
  let rooms = [];
  const msg = h('span');
  try {
    rooms = (await api('GET', '/v1/rooms')).rooms || [];
  } catch (err) {
    if (err.status === 401) throw err;
    msg.append(errorBox(err)); // the page itself still renders
  }
  let federation = false; // the entry shows only when the server has federation on
  try {
    await api('GET', '/v1/federation');
    federation = true;
  } catch (err) {
    if (err.status === 401) throw err;
  }
  return [
    h('a', { href: '#/' }, 'Rooms'), h('a', { href: '#/keys' }, 'Keys'), h('a', { href: '#/bots' }, 'Bots'), h('a', { href: '#/direct' }, 'Direct'),
    h('a', { href: '#/follows' }, 'Follows'), h('a', { href: '#/timeline' }, 'Timeline'),
    h('a', { href: '#/audit' }, 'Audit'), ...(federation ? [h('a', { href: '#/federation' }, 'Federation')] : []),
    h('a', { href: '#/passkeys' }, 'Passkeys'),
    h('span', { class: 'rooms' }, rooms.map(r => h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name))),
    msg,
    h('button', { onclick: action(msg, async () => { await logout(); await render(); }) }, 'Log out'),
  ];
}

// routes maps the address after # to a page. A page fills the element
// it gets and may return a function that stops what it started (the
// live stream of a room).
const routes = [
  [/^#\/?$/, roomsPage],
  [/^#\/rooms\/([^/]+)$/, roomPage],
  [/^#\/keys$/, keysPage],
  [/^#\/bots$/, botsPage],
  [/^#\/direct$/, directPage],
  [/^#\/follows$/, followsPage],
  [/^#\/timeline$/, timelinePage],
  [/^#\/audit$/, auditPage],
  [/^#\/federation$/, federationPage],
  [/^#\/passkeys$/, passkeysPage],
];

let leave = null; // stops the shown page's live stream
let seq = 0; // the newest render; an older one that finishes late shows nothing

async function render() {
  if (leave) { leave(); leave = null; }
  const mine = ++seq;
  const view = h('div');
  view.current = () => mine === seq; // false as soon as any newer render starts
  const show = (links = []) => {
    if (mine !== seq) return false;
    nav.replaceChildren(...links);
    main.replaceChildren(view);
    return true;
  };
  const hash = location.hash || '#/';
  try {
    const setup = /^#setup=(.+)$/.exec(hash);
    if (setup) {
      setupPage(view, setup[1]);
      return show();
    }
    const state = await api('GET', '/ui/auth/state');
    if (!state.logged_in) {
      loginPage(view, state);
      return show();
    }
    const links = await header();
    const route = routes.find(([re]) => re.test(hash));
    if (!route) {
      view.replaceChildren(h('h1', {}, 'Not found'), h('p', {}, h('a', { href: '#/' }, 'Back to the rooms')));
      return show(links);
    }
    const stop = await route[1](view, ...route[0].exec(hash).slice(1).map(decodeURIComponent));
    if (show(links)) leave = stop || null;
    else if (stop) stop();
  } catch (err) {
    if (err.status === 401) { // the session ended: idle, expired or logged out elsewhere
      try { loginPage(view, await api('GET', '/ui/auth/state')); } catch (e) { view.replaceChildren(errorBox(e)); }
    } else {
      view.replaceChildren(errorBox(err));
    }
    show();
  }
}

window.addEventListener('hashchange', render);
render();
