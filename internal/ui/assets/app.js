// The page: routing, the header, passkey login and setup, passkeys.
import { h, api, errorBox, confirmButton, table, when } from './lib.js';

const main = document.getElementById('app');
const nav = document.getElementById('nav');

// registerPasskey runs a registration ceremony: with a setup token for
// the first passkey, else as the logged-in operator.
async function registerPasskey(name, setupToken = '') {
  const options = await api('POST', '/ui/auth/register/begin', { name, setup_token: setupToken });
  const cred = await navigator.credentials.create({ publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(options) });
  await api('POST', '/ui/auth/register/finish', cred.toJSON());
}

async function login() {
  const options = await api('POST', '/ui/auth/login/begin');
  const cred = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(options) });
  await api('POST', '/ui/auth/login/finish', cred.toJSON());
}

// action makes a click handler that shows errors in msg.
function action(msg, fn) {
  return async () => {
    msg.replaceChildren();
    try { await fn(); } catch (err) { msg.replaceChildren(errorBox(err)); }
  };
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
    h('button', { onclick: action(msg, async () => { await login(); await render(); }) }, 'Log in with a passkey'),
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
      }),
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
          await api('DELETE', '/ui/auth/credentials/' + encodeURIComponent(c.id));
          await render();
        })
        : h('span', { class: 'muted' }, 'the last passkey cannot be removed'))))),
    h('h2', {}, 'Add a passkey'),
    h('label', {}, 'Name ', name), ' ',
    h('button', { onclick: action(msg, async () => { await registerPasskey(name.value); await render(); }) }, 'Add passkey'),
    msg);
}

// header returns the navigation: pages, the rooms, logout.
async function header() {
  const { rooms } = await api('GET', '/v1/rooms');
  const msg = h('span');
  return [
    h('a', { href: '#/' }, 'Rooms'), h('a', { href: '#/keys' }, 'Keys'),
    h('a', { href: '#/audit' }, 'Audit'), h('a', { href: '#/passkeys' }, 'Passkeys'),
    h('span', { class: 'rooms' }, (rooms || []).map(r => h('a', { href: '#/rooms/' + encodeURIComponent(r.id) }, r.name))),
    msg,
    h('button', { onclick: action(msg, async () => { await api('POST', '/ui/auth/logout'); await render(); }) }, 'Log out'),
  ];
}

// routes maps the address after # to a page. A page fills the element
// it gets and may return a function that stops what it started (the
// live stream of a room).
const routes = [
  [/^#\/passkeys$/, passkeysPage],
];

let leave = null; // stops the shown page's live stream
let seq = 0; // the newest render; an older one that finishes late shows nothing

async function render() {
  if (leave) { leave(); leave = null; }
  const mine = ++seq;
  const view = h('div');
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
