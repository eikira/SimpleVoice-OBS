'use strict';
// The listening address: the command line, the saved setting, the fallback and the page's calls.
// The program is copied to a temp folder (settings.txt lives next to it).
const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');
const BIN = process.env.RELAY_BIN || path.join(__dirname, '..', 'relay-test');
const SKIP = process.platform === 'win32';
const OPENER = process.platform === 'darwin' ? 'open' : 'xdg-open';
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

function sandbox() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'relay-net-'));
  const app = path.join(dir, 'app');
  fs.mkdirSync(app);
  const bin = path.join(app, 'relay');
  fs.copyFileSync(BIN, bin);
  fs.chmodSync(bin, 0o755);
  const fakeBin = path.join(dir, 'fakebin');
  fs.mkdirSync(fakeBin);
  const log = path.join(dir, 'opened.txt');
  fs.writeFileSync(path.join(fakeBin, OPENER), '#!/bin/sh\necho "$1" >> "' + log + '"\n', { mode: 0o755 });
  const env = Object.assign({}, process.env, { PATH: fakeBin + path.delimiter + process.env.PATH, HOME: dir, XDG_CONFIG_HOME: path.join(dir, 'cfg'),
    RELAY_NO_RESTART: '1', RELAY_UPDATE_URL: 'http://127.0.0.1:9/none' });
  return { dir, app, bin, log, env, settings: path.join(app, 'settings.txt') };
}

function cli(sb, args) {
  return new Promise((resolve) => {
    const p = spawn(sb.bin, args, { env: sb.env });
    let stdout = '', stderr = '';
    p.stdout.on('data', (c) => (stdout += c));
    p.stderr.on('data', (c) => (stderr += c));
    const t = setTimeout(() => p.kill(), 15000);
    p.on('close', (status) => { clearTimeout(t); resolve({ status, stdout, stderr }); });
  });
}

function req(base, method, p, headers, body) {
  return new Promise((resolve, reject) => {
    const r = http.request(base + p, { method, headers: headers || {} }, (res) => {
      let d = '';
      res.on('data', (c) => (d += c));
      res.on('end', () => resolve({ status: res.statusCode, body: d }));
    });
    r.on('error', reject);
    if (body) r.write(body);
    r.end();
  });
}

async function up(base) {
  for (let i = 0; i < 50; i++) { try { await req(base, 'GET', '/'); return; } catch (e) { await wait(100); } }
  throw new Error('relay did not start');
}

test('--list-addresses shows adapters and both reminders', { skip: SKIP }, async () => {
  const sb = sandbox();
  const r = await cli(sb, ['--list-addresses', '--port', '9123']);
  assert.strictEqual(r.status, 0);
  assert.match(r.stdout, /not a VPN/);
  assert.match(r.stdout, /same port in the extension/);
  assert.match(r.stdout, /port \(now 9123\)/);
});

test('--set-host: only addresses this computer has; not-private ones need the confirmation', { skip: SKIP }, async () => {
  const sb = sandbox();
  let r = await cli(sb, ['--set-host', '192.168.99.99']);
  assert.strictEqual(r.status, 2);
  assert.match(r.stdout, /no active adapter/);
  assert.ok(!fs.existsSync(sb.settings), 'nothing may be saved');
  r = await cli(sb, ['--set-host', 'banana']);
  assert.strictEqual(r.status, 2);
  r = await cli(sb, ['--set-host', '0.0.0.0']);
  assert.strictEqual(r.status, 2, '0.0.0.0 is not offered');
  r = await cli(sb, ['--set-host', '127.0.0.1']);
  assert.strictEqual(r.status, 0);
  assert.match(fs.readFileSync(sb.settings, 'utf8'), /HOST=127\.0\.0\.1/);
  // the real addresses of this machine (the test machine may have none that is private)
  const list = await cli(sb, ['--list-addresses']);
  const m = list.stdout.match(/^\s+(\d+\.\d+\.\d+\.\d+)\s+\S+\s+(.*)$/m);
  if (m) {
    const priv = m[2].trim() === 'private network';
    const plain = await cli(sb, ['--set-host', m[1]]);
    assert.strictEqual(plain.status, priv ? 0 : 2, m[1] + ' ' + plain.stdout);
    if (!priv) {
      const ok = await cli(sb, ['--set-host', m[1], '--yes-not-private']);
      assert.strictEqual(ok.status, 0);
      assert.match(fs.readFileSync(sb.settings, 'utf8'), new RegExp('HOST=' + m[1].replace(/\./g, '\\.')));
    }
  }
});

test('a saved address that does not exist any more: the relay starts on this PC only and says why', { skip: SKIP }, async () => {
  const sb = sandbox();
  fs.writeFileSync(sb.settings, 'HOST=198.51.100.77\r\nPORT=8787\r\n');
  const port = 20000 + Math.floor(Math.random() * 20000);
  const proc = spawn(sb.bin, ['--port', String(port)], { env: sb.env, stdio: 'ignore' });
  const base = 'http://127.0.0.1:' + port;
  try {
    await up(base);
    const d = JSON.parse((await req(base, 'GET', '/api/network')).body);
    assert.strictEqual(d.listening, '127.0.0.1');
    assert.strictEqual(d.saved, '198.51.100.77');
    assert.match(d.note, /198\.51\.100\.77/);
    assert.match(d.note, /this PC only/);
    const page = await req(base, 'GET', '/network');
    assert.strictEqual(page.status, 200);
    assert.match(page.body, /Do not choose a VPN/);
  } finally { proc.kill(); }
});

test('the page calls: refused without the header, refused for bad input, saved for a good address', { skip: SKIP }, async () => {
  const sb = sandbox();
  const port = 20000 + Math.floor(Math.random() * 20000);
  const proc = spawn(sb.bin, ['--port', String(port)], { env: sb.env, stdio: 'ignore' });
  const base = 'http://127.0.0.1:' + port;
  try {
    await up(base);
    let r = await req(base, 'POST', '/api/network/host', {}, '{"host":"127.0.0.1"}');
    assert.strictEqual(r.status, 403, 'a web page cannot add the custom header');
    r = await req(base, 'POST', '/api/network/host', { 'X-Relay-Open': '1' }, '{"host":"192.168.250.250"}');
    assert.strictEqual(r.status, 400);
    r = await req(base, 'POST', '/api/network/host', { 'X-Relay-Open': '1' }, '{"host":"0.0.0.0"}');
    assert.strictEqual(r.status, 400);
    assert.ok(!fs.existsSync(sb.settings));
    r = await req(base, 'POST', '/api/network/host', { 'X-Relay-Open': '1' }, '{"host":"127.0.0.1"}');
    assert.strictEqual(r.status, 200);
    assert.match(fs.readFileSync(sb.settings, 'utf8'), /HOST=127\.0\.0\.1/);
    await wait(900);
    await assert.rejects(req(base, 'GET', '/'), 'the relay closes after saving (the restart is switched off in this test)');
  } finally { proc.kill(); }
});

test('--open-network opens the page of this relay', { skip: SKIP }, async () => {
  const sb = sandbox();
  const r = await cli(sb, ['--open-network', '--port', '9321']);
  assert.strictEqual(r.status, 0);
  await wait(300);
  assert.match(fs.readFileSync(sb.log, 'utf8'), /http:\/\/127\.0\.0\.1:9321\/network/);
});
