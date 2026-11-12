// Shared helpers. Server data reaches the page only as text: h() turns
// strings into text nodes and attribute values, never into markup.

// h builds an element. attrs are attributes, except on<event> keys,
// which become listeners; true sets an empty attribute, false/null/
// undefined leave it out. Children are nodes or strings (text).
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) {
    if (c !== undefined && c !== null && c !== false) el.append(c);
  }
  return el;
}

// table builds a table with a header row.
export function table(headers, rows) {
  return h('table', {},
    h('thead', {}, h('tr', {}, headers.map(t => h('th', {}, t)))),
    h('tbody', {}, rows));
}

export class APIError extends Error {
  constructor(status, code, message) {
    super(`${code}: ${message}`);
    this.status = status;
    this.code = code;
  }
}

// session is this tab's session token. It lives only here, in the
// page's memory: never in a cookie (browsers send localhost cookies to
// every port, so another local process could collect it), nor in
// localStorage or sessionStorage (kept unencrypted on disk). A reload or
// a new tab starts logged out.
let session = '';

// setSession keeps the token a login or the first registration returned;
// '' logs the tab out.
export function setSession(token) {
  session = token || '';
}

// authHeaders is what a request needs to act as the logged-in operator.
export function authHeaders() {
  return session ? { 'X-Conclave-UI': session } : {};
}

// api calls the server as the logged-in operator: same origin (the
// passkey ceremony cookie) and the session token.
export async function api(method, path, body) {
  const opts = { method, headers: authHeaders(), credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res;
  for (let tries = 0; ; tries++) {
    res = await fetch(path, opts);
    // The rate limit (burst 30, then 10 a second) can refuse a page that
    // reads many rooms: wait as the server says and try again, 3 times.
    if (res.status !== 429 || tries === 3) break;
    await new Promise(r => setTimeout(r, 1000 * (Number(res.headers.get('Retry-After')) || 1)));
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = null; }
  if (!res.ok) {
    const e = (data && data.error) || {};
    throw new APIError(res.status, e.code || String(res.status),
      e.message || (data ? '' : text.trim().slice(0, 200)) || res.statusText || 'request failed');
  }
  return data;
}

// mapLimit runs fn on every item, at most limit at a time, and returns
// the results in order: a page does not fire a request per room at once.
export async function mapLimit(items, fn, limit = 4) {
  const out = [];
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return out;
}

// errorBox shows an error, for /v1 errors with its code and message.
export function errorBox(err) {
  return h('p', { class: 'error', role: 'alert' }, err instanceof Error ? err.message : String(err));
}

// when formats a server timestamp in local time; '' for none.
export function when(ts) {
  return ts ? new Date(ts).toLocaleString() : '';
}

// confirmButton is a button that asks in the page before action runs.
export function confirmButton(label, question, action) {
  const wrap = h('span', { class: 'confirm' });
  const reset = () => wrap.replaceChildren(h('button', { class: 'danger', onclick: ask }, label));
  function ask() {
    wrap.replaceChildren(question + ' ',
      h('button', { class: 'danger', onclick: run }, 'Yes'), ' ',
      h('button', { onclick: reset }, 'No'));
  }
  async function run() {
    try { await action(); } catch (err) {
      if (err.status === 401) rerender(); // the session ended: show the login page
      else wrap.replaceChildren(errorBox(err));
    }
  }
  reset();
  return wrap;
}

// action makes a click handler that shows errors in msg. While fn runs
// the button is disabled (no second run) and status, if given, is shown.
// A 401 means the session ended (show the login page) unless loggedIn is
// false: on the login and setup pages it is just a rejected passkey.
export function action(msg, fn, status = '', loggedIn = true) {
  return async e => {
    const btn = e.currentTarget;
    btn.disabled = true;
    msg.replaceChildren(status);
    try {
      await fn();
      msg.replaceChildren();
    } catch (err) {
      if (loggedIn && err.status === 401) rerender(); // the session ended: show the login page
      else msg.replaceChildren(errorBox(err));
    } finally {
      btn.disabled = false;
    }
  };
}

// Every right a membership can hold, in the server's order.
export const RIGHTS = ['read', 'claim', 'question', 'answer', 'decision', 'handoff',
  'status_update', 'task_assignment', 'alert', 'system'];

// rerender draws the current page again (app.js listens for this).
export function rerender() {
  window.dispatchEvent(new Event('hashchange'));
}

// rightsPicker is a checkbox per right; value() returns the checked ones.
export function rightsPicker(selected = []) {
  const boxes = RIGHTS.map(r => h('input', { type: 'checkbox', value: r, checked: selected.includes(r) }));
  return {
    el: h('span', { class: 'rights' }, boxes.map((b, i) => h('label', {}, b, ' ' + RIGHTS[i]))),
    value: () => boxes.filter(b => b.checked).map(b => b.value),
    set: rights => boxes.forEach(b => { b.checked = rights.includes(b.value); }),
  };
}

// showSecret replaces the page in view with a secret shown only once.
export function showSecret(view, title, secret) {
  const copyMsg = h('span', { role: 'alert' });
  const copy = h('button', {
    onclick: async () => {
      try {
        await navigator.clipboard.writeText(secret);
        copy.textContent = 'Copied';
      } catch {
        copyMsg.textContent = 'Copy failed — select the text and copy it manually';
      }
    },
  }, 'Copy');
  view.replaceChildren(h('h1', {}, title),
    h('div', { class: 'secret' }, h('pre', {}, secret), copy, copyMsg),
    h('p', {}, 'Shown once: the server keeps only a hash of the key.'),
    h('button', { onclick: rerender }, 'Done'));
}
