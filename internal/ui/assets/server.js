// Server: what runs (version, schema version, start time, federation)
// and the backup to take before an upgrade. Read-only: the backup holds
// the server's private federation key, so it is taken on the host with
// the CLI, never through this page.
import { h, api, when } from './lib.js';

const UPGRADING = 'https://github.com/ShaiSrc/conclave-mcp/blob/main/docs/upgrading.md';

export async function serverPage(view) {
  const s = await api('GET', '/v1/server');
  const f = s.federation || {};
  const row = (name, value) => h('tr', {}, h('th', {}, name), h('td', {}, value));
  view.replaceChildren(h('h1', {}, 'Server'),
    h('table', {}, h('tbody', {},
      row('Version', s.version),
      row('Schema version', String(s.schema_version)),
      row('Started', when(s.started_at)),
      row('Federation', f.enabled ? `on, domain ${f.domain}, mode ${f.mode}` : 'off'),
      row('Protocol', f.protocol))),
    h('h2', {}, 'Backup'),
    h('p', {}, 'Take one before every upgrade. Run it on this machine as the user the server runs as; the server keeps running. ' +
      'Add --db with the file the server runs on (its serve --db) unless that is ./.conclave-mcp/data.db.'),
    h('pre', {}, 'conclave-mcp backup /path/to/conclave-backup.db'),
    h('p', { class: 'muted' }, "The copy holds the server's private federation key and the key hashes: keep it as safe as the data directory."),
    h('p', {}, 'How to upgrade: ', h('a', { href: UPGRADING, target: '_blank', rel: 'noopener noreferrer' }, 'the upgrade guide'), '.'));
}
