'use strict';
// Relay protocol 2: intro clip, chat lines, /info. Starts the compiled program like the other tests.
const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const path = require('path');
const { spawn } = require('child_process');
const BIN = process.env.RELAY_BIN || path.join(__dirname, '..', 'relay-test');
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const ID = 'protocol2test1234';

let proc, base;
function call(method, p, body) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? null : JSON.stringify(body);
    const r = http.request(base + p, { method, headers: data ? { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) } : {} }, (res) => {
      let d = ''; res.on('data', (c) => (d += c)); res.on('end', () => resolve({ status: res.statusCode, json: (() => { try { return JSON.parse(d); } catch (e) { return null; } })() }));
    });
    r.on('error', reject); if (data) r.write(data); r.end();
  });
}
function listen() {
  const got = [];
  const req = http.get(base + '/events?id=' + ID, (res) => {
    let buf = '';
    res.on('data', (c) => { buf += c; let i; while ((i = buf.indexOf('\n\n')) >= 0) { const block = buf.slice(0, i); buf = buf.slice(i + 2); const m = /^data: (.*)$/m.exec(block); if (m) got.push(JSON.parse(m[1])); } });
  });
  return { got, close: () => req.destroy() };
}
const base0 = { endpoint: 'http://127.0.0.1:3900/v1/audio/speech', voice: 'v1', text: 'hello there', model: 'm', format: 'wav' };

test.before(async () => {
  const port = 20000 + Math.floor(Math.random() * 20000);
  base = 'http://127.0.0.1:' + port;
  proc = spawn(BIN, ['--port', String(port)], { stdio: 'ignore', env: Object.assign({}, process.env, { RELAY_UPDATE_URL: 'http://127.0.0.1:9/none' }) });
  for (let i = 0; i < 50; i++) { try { await call('GET', '/'); return; } catch (e) { await wait(100); } }
  throw new Error('relay did not start');
});
test.after(() => proc && proc.kill());

test('/info tells the protocol version (the extension needs 2 or higher)', async () => {
  const r = await call('GET', '/info');
  assert.strictEqual(r.json.protocol, 2);
  assert.match(r.json.version, /^\d+\.\d+\.\d+$/);
});

test('intro, kind and maxQueue are passed on; speed and model of the intro are kept', async () => {
  const l = listen(); await wait(300);
  const r = await call('POST', '/send?id=' + ID, Object.assign({}, base0, { kind: 'chat', maxQueue: 4, intro: { text: 'Paul says', voice: 'announcer', speed: 1.1, model: 'm2' } }));
  assert.strictEqual(r.status, 200);
  await wait(300); l.close();
  assert.strictEqual(l.got.length, 1);
  assert.deepStrictEqual(l.got[0].intro, { text: 'Paul says', voice: 'announcer', model: 'm2', speed: 1.1 });
  assert.strictEqual(l.got[0].kind, 'chat');
  assert.strictEqual(l.got[0].maxQueue, 4);
});

test('a plain command message looks exactly like before (no intro, no kind)', async () => {
  const l = listen(); await wait(300);
  await call('POST', '/send?id=' + ID, Object.assign({}, base0, { command: 'voice' }));
  await wait(300); l.close();
  assert.strictEqual(l.got[0].intro, undefined);
  assert.strictEqual(l.got[0].kind, undefined);
  assert.strictEqual(l.got[0].maxQueue, undefined);
  assert.strictEqual(l.got[0].command, 'voice');
});

test('a broken intro makes the whole message invalid (never half-spoken)', async () => {
  for (const bad of [{ text: '', voice: 'a' }, { text: 'x', voice: '' }, { text: 'x'.repeat(301), voice: 'a' }, 'text', 5, [], { voice: 'a' }]) {
    const r = await call('POST', '/send?id=' + ID, Object.assign({}, base0, { intro: bad }));
    assert.strictEqual(r.status, 400, JSON.stringify(bad));
  }
});

test('kind other than chat is ignored; maxQueue only counts for chat lines and within 1-100', async () => {
  const l = listen(); await wait(300);
  await call('POST', '/send?id=' + ID, Object.assign({}, base0, { kind: 'evil', maxQueue: 5 }));
  await call('POST', '/send?id=' + ID, Object.assign({}, base0, { kind: 'chat', maxQueue: 5000 }));
  await call('POST', '/send?id=' + ID, Object.assign({}, base0, { kind: 'chat', maxQueue: 0 }));
  await wait(400); l.close();
  assert.strictEqual(l.got[0].kind, undefined); assert.strictEqual(l.got[0].maxQueue, undefined);
  assert.strictEqual(l.got[1].kind, 'chat'); assert.strictEqual(l.got[1].maxQueue, undefined);
  assert.strictEqual(l.got[2].maxQueue, undefined);
});
