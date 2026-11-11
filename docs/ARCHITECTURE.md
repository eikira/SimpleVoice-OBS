# Architecture

SimpleVoice OBS has three parts that ship together in one installer.

```
 Chrome extension ──HTTP──►  SimpleVoice OBS Relay (Go, one exe, tray icon)  ──HTTP + token──► worker.py (Python)
 (VoiceStudio+SimpleVoice          │   control panel (panel.html)                                  ├─ OmniVoice backend (GPU, cloning)
  OBS Connector)                   │   OBS overlay (overlay.html / overlay.js)                     ├─ Kokoro backend (CPU, built-in voices)
 OBS Browser Source ──────────────►│   voices folder (WAV + TXT pairs)                             └─ Whisper (transcription)
                                   └─ ffmpeg (conversion, saving)
 Setup (Go, one exe) installs all of the above, plus Python, PyTorch and the models.
```

| Directory | What it is |
|---|---|
| `relay/` | The relay program (Go, standard library only) and the engine worker `worker.py` |
| `setup/` | The installer (Go, standard library only). It embeds the relay exe, `worker.py` and the licence files from `setup/payload/` |
| `docs/` | This file |
| `build.sh`, `test.sh` | Build and check everything |

## The relay (`relay/`)

| File | Purpose |
|---|---|
| `main.go` | Command line flags, settings file, start-up. `version` constant. |
| `server.go` | The extension and OBS protocol: `/overlay.html`, `/overlay.js`, `/events` (SSE), `/send`, `/status`, `/tts` (proxy to VoiceStudio), `/info`, `/open-folder`, `/shutdown`. `protocolVersion = 2`. |
| `speech.go` | VoiceStudio-compatible API (`/v1/audio/speech`, `/v1/audio/voices`, `/v1/models`) and the panel API (`/api/...`). |
| `engine.go` | Starts, supervises and talks to `worker.py`. States: starting, ready, error. Modes: `auto`, `gpu`, `cpu`. Automatic fallback to the processor engine when the graphics card engine fails. |
| `voices.go` | The voices folder: a voice is `Name.wav` plus `Name.txt` (the exact words). Import checks, rename, listing. |
| `audio.go` | ffmpeg: convert imports to mono WAV, and save formats (MP3, WAV, FLAC, AAC: mono, 24 kHz). |
| `tray_windows.go`, `tray_other.go` | Tray icon, menu, autostart, log, data folder (`%LOCALAPPDATA%\SimpleVoiceOBS`). |
| `panel.html` | The control panel (one file, no libraries). |
| `overlay.html`, `overlay.js` | The OBS Browser Source page that plays the audio. |
| `worker.py` | The engine worker (see below). |
| `tests/*.test.js` | Extension protocol tests (Node). `engine_test.go`: everything else, with a fake engine. |

### Panel API and its guard
Panel endpoints need the header `X-Relay-Panel: 1`, a loopback `Host`, and a matching `Origin` (`guardPanel`). A web page on another
site cannot send that header cross-origin. Responses use `writeJSONNoCORS` and `speechError`.

| Endpoint | Purpose |
|---|---|
| `GET /api/status` | Everything the panel shows: engine, voices, steps, language, numbers, languages, save formats, sample voice names |
| `POST /api/voices/draft`, `/api/voices`, `/api/voices/draft/discard` | Import: upload, review the words, save or cancel |
| `POST /api/voices/transcribe`, `/transcript`, `/delete`, `/rename` | Voice maintenance |
| `POST /api/engine/mode`, `/quality`, `/options`, `/restart` | Engine settings (saved in `settings.txt`) |
| `POST /api/convert?format=mp3\|flac\|aac` | Convert a generated WAV for saving |
| `POST /api/open-voices` | Open the voices folder |

### Settings (`settings.txt`, next to the exe)
`PORT` (8787), `HOST` (127.0.0.1), `VOICES`, `ENGINEDIR`, `FFMPEG`, `ENGINE` (auto/gpu/cpu), `STEPS` (16/32/48, default 32),
`LANGUAGE` (`auto` or a code), `NUMBERS` (`on`/`off`). Writes are serialized (`writeSetting`, one mutex).

## The worker (`relay/worker.py`)

A `ThreadingHTTPServer` on `127.0.0.1` with the header `X-Worker-Token`. Endpoints: `/health`, `/voices`, `/speak`, `/transcribe`.
Backends: `OmniBackend` (GPU, fp16, cloning), `KokoroBackend` (CPU, ONNX), `FakeBackend` (tests, `OMNIVOICE_FAKE=1`; also `_NOGPU`, `_FAIL`, `_SLOW`).
It also makes the sample voices (`--make-starters <dir> --kokoro <dir> --only A,B`).

Important behaviours:
- **Speed** on the graphics card engine is OmniVoice's native `speed`, which changes the target length. At 0.7 and below the model repeats parts of an imported recording; the relay therefore refuses to go below 0.8 for imported voices (`minCloneSpeed`). Stretching the finished audio with ffmpeg `atempo` was tried and rejected (sounded bad, did not fix it).
- **Quality** is `num_step` (16/32/48). Kokoro has no steps.
- **Language** is passed to OmniVoice only when set. **Numbers as words** (`spoken_numbers`) is our own English implementation, because OmniVoice's text normalizer needs another library. Digit runs longer than 12 digits are left as they are. Text in `[brackets]` is never touched.
- **Audio** from every engine is mono, 16-bit, 24 kHz WAV.
- **Loading** logs progress, and a stall report (`faulthandler`, default 45 s, `OMNIVOICE_DUMP_AFTER`) is written when loading hangs.

### The Windows deadlock (do not reintroduce)
A worker thread blocked in a synchronous read on the stdin pipe froze NumPy/OpenBLAS DLL initialization on Windows, so the model never
finished loading. On Windows the worker therefore gets **no stdin pipe**: it watches the relay's process handle (`--parent <pid>`,
`OpenProcess` + `WaitForSingleObject`) and exits when the relay is gone. The relay stops it with `Process.Kill`. On other systems stdin EOF is still used.
While loading, the server handles requests on the main thread (`busy_loading`) instead of starting threads.

## Setup (`setup/`)

A local HTTP server plus a browser `--app` window showing `ui.html` (no browser tabs). Guard: loopback `Host` and header `X-Setup: 1`.
A native Windows wizard was planned and is parked; the browser version is the production one.

| File | Purpose |
|---|---|
| `main.go` | Server, window, endpoints `/state /plan /start /uninstall /launch /quit` |
| `install.go` | The install steps, downloads with SHA-256 checks and retries, rollback on a failed clean install |
| `components.go` | Component list with size estimates, hardware detection and the graphics card guide |
| `inventory.go` | `inventory.json` and the sample voice selection |
| `app.go` | Program files, shortcuts, Windows Apps list and autostart entries, names |
| `proxy.go` | Loopback proxy so pip and the model downloader work behind restrictive networks |
| `ui.html` | The setup page |
| `disk_*.go`, `sysproc_*.go` | Free space and hidden windows |

Install steps (only those selected): check GPU, install program, Python 3.11.9 (embeddable), PyTorch 2.8.0+cu128, OmniVoice 0.2.1, ffmpeg
(imageio-ffmpeg), processor engine (kokoro-onnx), check PyTorch, download models (OmniVoice, Kokoro files, Whisper large-v3-turbo), make sample voices, shortcuts.
Install folder `%LOCALAPPDATA%\SimpleVoice-OBS`; the relay's data folder is `%LOCALAPPDATA%\SimpleVoiceOBS` (log, engine script).

### Graphics card rule
`detectGPU` asks `nvidia-smi` for name, memory and `compute_cap`. The bundled PyTorch supports compute capability **7.0 and newer**
(GTX 16 series, RTX 20 series and up). No usable card means: processor engine only, no PyTorch, no OmniVoice, no Whisper, no sample voice list
(enforced in `handleStart`, not only on the page). Below 6 GB video memory only a warning is shown.

### The inventory (`inventory.json`)
Written after a successful install: the build number, the files Setup placed (relative paths), the sample voices Setup made with the SHA-256 of their `.wav` and `.txt`,
and the voices chosen last time. Rules:
1. An update removes files that the old inventory lists and the new build does not ship, only inside the install folder.
2. Voices, settings and unlisted files are never removed. Without a readable inventory nothing is removed.
3. A sample voice is removed when unticked only if Setup made it (it is in the inventory) and both files still match their hashes. Renamed, edited or imported voices are never removed.
4. The inventory is written only after success (voice records are saved as soon as voices are made).
5. Uninstall removes the listed files and the inventory; voices stay unless the user asks.

## Tests

`./test.sh` runs: `gofmt`, `go vet` (Linux and Windows), `go test -race` for relay and setup, and the Node protocol tests. The real models never run in tests.
Mutation checks are part of the working method: after adding a safeguard, break it on purpose and confirm a test fails.
