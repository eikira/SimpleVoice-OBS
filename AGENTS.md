# AGENTS.md: rebuild and maintenance guide

Read this first if you are an AI assistant (or a developer) taking over SimpleVoice OBS with only this repository.
Together with the source, this file is enough to continue the project. Details of the design are in `docs/ARCHITECTURE.md`.

## 1. What the product is
**SimpleVoice OBS** lets a streamer type text and have it spoken in a chosen voice into OBS. A Windows installer (Setup) installs a
background program (the Relay) and an engine (Python, PyTorch, OmniVoice for the graphics card, Kokoro for the processor, Whisper for transcribing).
Owner: eikira. Licence: GPL-3.0. Version 1.0.0 is the first production release.

## 2. Names (use exactly these)
| Thing | Name |
|---|---|
| Product | SimpleVoice OBS |
| Background program | SimpleVoice OBS Relay (`SimpleVoiceOBSRelay.exe`, tray tooltip) |
| Installer | `SimpleVoice-OBS-Setup.exe` (shipped in `SimpleVoice-OBS-Setup-<version>.zip`) |
| Chrome extension | VoiceStudio+SimpleVoice OBS Connector (formerly "VoiceStudio OBS Connector"; the rename and re-release are a separate, later job) |
| Standalone relay for VoiceStudio | VoiceStudio OBS Connector Relay (not renamed; may get a tiny rebuild for the extension name) |
| Voice model | OmniVoice (this stays the engine name) |
| Install folder | `%LOCALAPPDATA%\SimpleVoice-OBS` |
| Data folder | `%LOCALAPPDATA%\SimpleVoiceOBS` (`relay.log`) |
| Registry | `...\Uninstall\SimpleVoiceOBS`, Run value `SimpleVoiceOBS` |
| Shortcut | `SimpleVoice OBS.lnk` |

## 3. Owner rules (follow these)
- Standard library only for Go. No frameworks, no bloat. Manual updates only: the relay and Setup never contact the internet by themselves (`updateCheck:false`).
- User-facing text: short and plain. Say plainly what is untested.
- Do exactly what is asked. "note" or "don't do anything yet" means acknowledge only. Do not add unrequested files or features.
- Do not ask the same question twice. Reply in English.
- Imported voices must not go below 0.8x speed; the 13 sample voices keep 0.5x. This was asked for explicitly.
- Setup is delivered as a zip. The source goes to GitHub with documentation.
- No AMD support (parked; could not be tested). No graphics card older than GTX 16 series (see section 5).

## 4. Version scheme
First production release is 1.0.0. Updates go in 0.0.1 steps: 1.0.1 … 1.0.9, then 1.1.0. Update `const version` in `relay/main.go`
and `setupBuild` in `setup/main.go`, add a `CHANGELOG.md` entry, run `./test.sh`, then `./build.sh`.
Test builds before 1.0.0 are not supported: there is no migration code, testers uninstall them first.

## 5. Decisions and the reasons for them
- **Graphics card**: PyTorch 2.8.0+cu128 supports compute capability 7.0 and up (GTX 16, RTX 20 and newer). GTX 10 (Pascal) is unsupported; wheels for cu126 would support it but are not built. Without a usable card, Setup installs the processor engine only (about 0.5 GB on disk instead of 11.4 GB).
- **Speed floor 0.8 for imported voices**: below it OmniVoice repeats parts of the recording (found by the owner testing at 0.5 and 0.7; 0.8 was clean). Applies by voice name: sample voice names are exempt (`isSampleVoice`, `minCloneSpeed` in `relay/speech.go`). Renaming a sample voice makes it count as imported.
- **No stdin pipe to the worker on Windows**: see the deadlock section of `docs/ARCHITECTURE.md`.
- **Own number-to-words** (`spoken_numbers`): English only, GPU engine only, off by default.
- **Inventory**: Setup only removes what it recorded itself. See `docs/ARCHITECTURE.md`.
- **Sample voice list** exists in three places that must match: `STARTERS` in `relay/worker.py`, `sampleVoiceNames` in the relay, `sampleVoices` in Setup. Drift tests guard it.
- **Browser window for Setup**: works, but the browser's translate popup can appear. A native Win32 wizard would fix that; parked.

## 6. How to build and check
Needs Go 1.24+, Node 18+, Python 3 (for syntax check), bash. Nothing else.
```
./test.sh     # gofmt, vet (Linux + Windows), go test -race (relay, setup), node protocol tests. Must end with ALL CHECKS PASSED
./build.sh    # dist/SimpleVoice-OBS-Setup.exe (Windows amd64, windowsgui, -trimpath -ldflags "-s -w -H windowsgui")
```
`build.sh` builds the relay exe into `setup/payload/`, copies `worker.py`, `LICENSE`, `THIRD-PARTY-NOTICES.txt` there, then builds Setup which embeds `payload/` (`//go:embed all:payload`).
The `.syso` files carry the icon and manifest; they are committed. Run Node tests one file at a time with `RELAY_BIN=<path of a test build of the relay>` (running the directory fails).
Release zips: `SimpleVoice-OBS-Setup-<version>.zip` containing the exe; the source zip contains no binaries and no `setup/payload` contents.

## 7. Test conventions
- Use `httptest` and the fake engine (`OMNIVOICE_FAKE=1`; variants `_NOGPU`, `_FAIL`, `_SLOW`). Do not run real models in tests.
- After adding a safeguard, do a mutation check: break it, confirm a test fails, restore it.
- Never print huge strings from tests.

## 8. Environment limits of an AI sandbox
No Windows, no GPU, no access to Hugging Face. So these can **not** be tested there: the installer on real Windows, real model loading and speech quality, the tray icon and autostart, the browser window. The owner tests on an RTX 3050 (4 GB). Always list untested items in the release notes.
Verified by the owner on test builds: the graphics card engine loads and speaks, voice import and cloning work, the processor engine and its speed changes work, the uninstaller worked. Not verified: see `CHANGELOG.md`.

## 9. Pinned versions
Python 3.11.9 embeddable, torch and torchaudio 2.8.0+cu128, `omnivoice==0.2.1`, `kokoro-onnx==0.6.1`, Whisper large-v3-turbo, imageio-ffmpeg. Size figures in Setup (`components.go`) are estimates; update them when versions change.

## 10. Parked work
1. Rename the Chrome extension to VoiceStudio+SimpleVoice OBS Connector and add a switch between VoiceStudio and SimpleVoice (the address setting; relay default `http://127.0.0.1:8787`). Needs the extension source; protocolVersion is 2 and `relay/tests` document the protocol.
2. Tiny rebuild of the standalone VoiceStudio OBS Connector Relay for the connector name in its texts.
3. Native Win32 setup wizard.
4. cu126 build for GTX 10 cards. AMD support (needs a tester).

## 11. Release procedure
1. Change code, run `./test.sh`.
2. Bump versions (section 4), update `CHANGELOG.md` and, if behaviour changed, `docs/ARCHITECTURE.md`, `README.md` and this file.
3. `./build.sh`, zip the exe, zip the source, give both to the owner. The owner tests, then publishes to GitHub as a release (tag `v1.0.0`, attach the Setup zip).
