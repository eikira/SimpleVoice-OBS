'use strict';
const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const { spawn } = require('child_process');
const BIN = process.env.RELAY_BIN || require('path').join(__dirname, '..', 'relay-test');
let proc;

const ID = 'testid1234567890';
let base;

function request(method, path, body) {
  return new Promise((resolve, reject) => {
    const req = http.request(base + path, { method, headers: body ? { 'Content-Type': 'application/json' } : {} }, (res) => {
      let data = '';
      res.on('data', (c) => (data += c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: data }));
    });
    req.on('error', reject);
    if (body !== undefined) req.write(typeof body === 'string' ? body : JSON.stringify(body));
    req.end();
  });
}

function listen(path) {
  return new Promise((resolve) => {
    const events = [];
    const req = http.get(base + path, (res) => {
      res.setEncoding('utf8');
      res.on('data', (chunk) => {
        chunk.split('\n\n').forEach((block) => {
          const line = block.split('\n').find((l) => l.startsWith('data: '));
          if (line) events.push(JSON.parse(line.slice(6)));
        });
      });
      resolve({ req, res, events });
    });
  });
}

const good = { endpoint: 'http://127.0.0.1:3900/v1/audio/speech', model: 'tts-1-hd', voice: 'hazed', format: 'wav', text: 'hello', speed: 1.1, user: 'Jess', command: 'hazed' };
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

test.before(async () => {
  const port = 20000 + Math.floor(Math.random() * 20000);
  proc = spawn(BIN, ['--port', String(port)], { stdio: 'ignore', env: Object.assign({}, process.env, { RELAY_UPDATE_URL: 'http://127.0.0.1:9/none' }) });
  base = 'http://127.0.0.1:' + port;
  for (let i = 0; i < 50; i++) { try { await request('GET', '/'); return; } catch (e) { await wait(100); } }
  throw new Error('relay did not start');
});
test.after(() => { proc.kill(); });

test('delivers a posted command to a connected listener', async () => {
  const l = await listen('/events?id=' + ID);
  await wait(50);
  assert.deepStrictEqual(JSON.parse((await request('GET', '/status?id=' + ID)).body), { ok: true, listeners: 1 });
  const r = await request('POST', '/send?id=' + ID, good);
  assert.strictEqual(r.status, 200);
  assert.deepStrictEqual(JSON.parse(r.body), { ok: true, listeners: 1 });
  await wait(100);
  assert.strictEqual(l.events.length, 1);
  assert.strictEqual(l.events[0].text, 'hello');
  assert.strictEqual(l.events[0].voice, 'hazed');
  assert.strictEqual(l.events[0].speed, 1.1);
  l.req.destroy();
});

test('does not leak to listeners with a different id', async () => {
  const other = await listen('/events?id=otherid1234567890');
  await wait(50);
  const r = await request('POST', '/send?id=' + ID, good);
  assert.strictEqual(JSON.parse(r.body).listeners, 0);
  await wait(100);
  assert.strictEqual(other.events.length, 0);
  other.req.destroy();
});

test('listener count drops when OBS disconnects', async () => {
  const l = await listen('/events?id=' + ID);
  await wait(50);
  l.req.destroy();
  await wait(150);
  assert.strictEqual(JSON.parse((await request('GET', '/status?id=' + ID)).body).listeners, 0);
});

test('rejects bad ids and bad payloads', async () => {
  assert.strictEqual((await request('POST', '/send?id=x', good)).status, 400);
  assert.strictEqual((await request('GET', '/events')).status, 400);
  assert.strictEqual((await request('POST', '/send?id=' + ID, '{not json')).status, 400);
  assert.strictEqual((await request('POST', '/send?id=' + ID, Object.assign({}, good, { text: '' }))).status, 400);
  assert.strictEqual((await request('POST', '/send?id=' + ID, Object.assign({}, good, { endpoint: 'file:///etc/passwd' }))).status, 400);
  assert.strictEqual((await request('POST', '/send?id=' + ID, Object.assign({}, good, { voice: 'x'.repeat(300) }))).status, 400);
  assert.strictEqual((await request('POST', '/send?id=' + ID, 'x'.repeat(20000))).status, 413);
});

test('drops out-of-range speed and unsafe format instead of forwarding them', async () => {
  const l = await listen('/events?id=' + ID);
  await wait(50);
  await request('POST', '/send?id=' + ID, Object.assign({}, good, { speed: 99, format: '../../x' }));
  await wait(100);
  assert.strictEqual(l.events[0].speed, undefined);
  assert.strictEqual(l.events[0].format, 'wav');
  l.req.destroy();
});

function upstream(handler) {
  return new Promise((resolve) => {
    const seen = [];
    const s = http.createServer((req, res) => {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => { seen.push({ url: req.url, body: JSON.parse(body) }); handler(req, res); });
    });
    s.listen(0, '127.0.0.1', () => resolve({ s, seen, url: 'http://127.0.0.1:' + s.address().port + '/v1/audio/speech' }));
  });
}

test('proxy: forwards a speech request to VoiceStudio (which sends no CORS headers) and returns the audio', async () => {
  const up = await upstream((req, res) => { res.setHeader('Content-Type', 'audio/wav'); res.end(Buffer.from('RIFFfakewav')); });
  const l = await listen('/events?id=' + ID);
  await wait(50);
  const body = { model: 'tts-1-hd', input: 'hi', voice: 'hazed', response_format: 'wav', speed: 1.1 };
  const r = await request('POST', '/tts?id=' + ID, { endpoint: up.url, body });
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.headers['content-type'], 'audio/wav');
  assert.strictEqual(r.headers['access-control-allow-origin'], '*');
  assert.strictEqual(r.body, 'RIFFfakewav');
  assert.deepStrictEqual(up.seen[0].body, body);
  l.req.destroy(); up.s.close();
});

test('proxy: passes VoiceStudio error status through', async () => {
  const up = await upstream((req, res) => { res.writeHead(500); res.end('model not found'); });
  const l = await listen('/events?id=' + ID);
  await wait(50);
  const r = await request('POST', '/tts?id=' + ID, { endpoint: up.url, body: { input: 'x' } });
  assert.strictEqual(r.status, 500);
  assert.strictEqual(r.body, 'model not found');
  l.req.destroy(); up.s.close();
});

test('proxy: unreachable VoiceStudio gives a clear 502 message', async () => {
  const tmp = http.createServer();
  await new Promise((r) => tmp.listen(0, '127.0.0.1', r));
  const port = tmp.address().port;
  await new Promise((r) => tmp.close(r));
  const l = await listen('/events?id=' + ID);
  await wait(50);
  const r = await request('POST', '/tts?id=' + ID, { endpoint: 'http://127.0.0.1:' + port + '/v1/audio/speech', body: { input: 'x' } });
  assert.strictEqual(r.status, 502);
  assert.match(JSON.parse(r.body).error, /Could not reach VoiceStudio at 127\.0\.0\.1:\d+ \(ECONNREFUSED\)/);
  l.req.destroy();
});

test('proxy: works without an OBS page connected (settings Test), refuses non-speech endpoints and bad ids', async () => {
  const up = await upstream((req, res) => { res.setHeader('Content-Type', 'audio/wav'); res.end(Buffer.from('RIFFok')); });
  const noListener = await request('POST', '/tts?id=nobodyhere123456', { endpoint: up.url, body: { input: 'x' } });
  assert.strictEqual(noListener.status, 200);
  assert.strictEqual(noListener.body, 'RIFFok');
  up.s.close();
  assert.strictEqual((await request('POST', '/tts?id=short', { endpoint: up.url, body: {} })).status, 400);
  assert.strictEqual((await request('POST', '/tts?id=' + ID, { endpoint: 'http://127.0.0.1:3900/admin/delete', body: {} })).status, 400);
  assert.strictEqual((await request('POST', '/tts?id=' + ID, { endpoint: 'file:///etc/passwd', body: {} })).status, 400);
  assert.strictEqual((await request('POST', '/tts?id=' + ID, { endpoint: 'http://127.0.0.1:3900/v1/audio/speech' })).status, 400);
});

test('serves the overlay and allows CORS', async () => {
  const page = await request('GET', '/overlay.html');
  assert.strictEqual(page.status, 200);
  assert.match(page.body, /overlay\.js/);
  assert.strictEqual((await request('GET', '/overlay.js')).status, 200);
  assert.strictEqual((await request('GET', '/server.js')).status, 404); // source is not served
  const pre = await request('OPTIONS', '/send?id=' + ID);
  assert.strictEqual(pre.status, 204);
  assert.strictEqual(pre.headers['access-control-allow-origin'], '*');
});

test('shutdown: refused without the stop header, closes the program with it', async () => {
  assert.strictEqual((await request('POST', '/shutdown')).status, 403);
  const stopped = new Promise((r) => proc.on('exit', r));
  const r = await new Promise((resolve, reject) => {
    const req = http.request(base + '/shutdown', { method: 'POST', headers: { 'X-Relay-Stop': '1' } }, (res) => { res.resume(); resolve(res.statusCode); });
    req.on('error', reject); req.end();
  });
  assert.strictEqual(r, 200);
  await stopped;
});
