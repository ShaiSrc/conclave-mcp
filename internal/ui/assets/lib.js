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

// api calls the server as the logged-in operator: same origin, the
// session cookie, and the header /v1 needs to accept the cookie.
export async function api(method, path, body) {
  const opts = { method, headers: { 'X-Conclave-UI': '1' }, credentials: 'same-origin' };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = null; }
  if (!res.ok) {
    const e = data && data.error ? data.error : { code: String(res.status), message: text.trim() || res.statusText };
    throw new APIError(res.status, e.code, e.message);
  }
  return data;
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
    try { await action(); } catch (err) { wrap.replaceChildren(errorBox(err)); }
  }
  reset();
  return wrap;
}
