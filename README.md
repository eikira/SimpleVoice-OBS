# SimpleVoice OBS

Read your stream chat aloud in a cloned voice, on your own PC, with OBS. Free and open source (GPL-3.0).

SimpleVoice OBS is one installer that sets up everything: a small background program (the **SimpleVoice OBS Relay**), a
voice engine, and a control panel in your browser where you add voices and test them. It works together with the
**VoiceStudio+SimpleVoice OBS Connector** Chrome extension and the OBS Browser Source, exactly as the VoiceStudio setup
does, so nothing else has to change.

> **Non-commercial use only.** The voice model (OmniVoice) is licensed CC-BY-NC. Only clone voices you are allowed to clone.

## What you get

- **Voice cloning** from a short recording (3 to 10 seconds works best, up to 20 seconds is accepted), on an NVIDIA graphics card.
- **Processor engine** that runs on any PC, with built-in voices (no cloning).
- **13 sample voices** made during setup (you choose which), usable for cloning-quality speech right away.
- **Control panel** with a voice list, adding and renaming voices, automatic transcription of a new recording, a Test section
  (Generate, Play/Stop, Save as MP3, WAV, FLAC or AAC), voice quality, language and "write numbers as words".
- **Works with the extension you already use.** The relay answers the same requests as VoiceStudio:
  `/overlay.html`, `/events`, `/send`, `/status`, `/tts`, `/info`, `/open-folder`, `/shutdown` and
  `/v1/audio/speech`, `/v1/audio/voices`, `/v1/models`.
- **Private.** Everything runs on your PC. The relay only listens on `127.0.0.1`. The internet is used while installing (see below).

## Requirements

| | |
|---|---|
| System | Windows 10 or 11, 64 bit |
| Voice cloning | NVIDIA GTX 16 series, RTX 20 series or newer, with 6 GB video memory or more |
| Any other PC | The processor engine is installed instead: built-in voices, no cloning, no transcription |
| Disk space | about 11.4 GB for everything, about 0.5 GB for the processor engine only (estimates) |
| Download | about 8.3 GB for everything, about 0.26 GB for the processor engine only (estimates) |

Older NVIDIA cards (GTX 10 series and before) and AMD or Intel graphics cards cannot use the graphics card engine.
Setup detects this and installs only the processor engine, without downloading anything that only the graphics card engine needs.

## Install

1. Download `SimpleVoice-OBS-Setup-<version>-Windows.zip` from the releases page and unzip it.
2. Double-click `SimpleVoice-OBS-Setup.exe`, tick what you want, accept the licence note, click **Install**.
3. When it finishes, the control panel opens. Add a voice and press **Generate** in the Test section.
4. In the **VoiceStudio+SimpleVoice OBS Connector** extension settings set *VoiceStudio address* to `http://127.0.0.1:8787`.
   Keep your OBS Browser Source URL as it is.

Setup needs no administrator rights. It installs to `%LOCALAPPDATA%\SimpleVoice-OBS`. If Windows or your firewall asks,
click Allow for the program and for `python.exe`.

During install, Setup downloads from python.org, pypi.org and files.pythonhosted.org, download.pytorch.org,
huggingface.co and github.com. Nothing is sent anywhere. The relay is designed to work offline after install (not verified on every setup).

## Using it

- Tray icon: double-click opens the control panel. The menu has: control panel, voices folder, log, change port, exit.
- **Imported voices** go down to speed 0.8 only. Slower speeds make the model repeat parts of the recording.
  The sample voices and the processor engine's built-in voices go from 0.5 to 1.5.
- **Voice quality** (graphics card engine): Fast 16 steps, Normal 32 steps (default), High 48 steps.
- Text for one message can be up to 4000 characters. Very long text is not tested.

## Updating and removing

Updates are manual: run the newer Setup. It keeps your voices and settings, replaces the program, and removes files
the new version no longer ships (it keeps a list of what it installed in `inventory.json`).
To remove everything, open the Setup again and use **Uninstall**, or use Windows *Apps*. Your voices are kept unless you choose to delete them.

## Names

| Name | What it is |
|---|---|
| **SimpleVoice OBS** | this whole product |
| **SimpleVoice OBS Relay** | the background program (`SimpleVoiceOBSRelay.exe`) |
| **VoiceStudio+SimpleVoice OBS Connector** | the Chrome extension (until it is re-released the installed version, 0.1.1, still says "VoiceStudio OBS Connector") |
| **VoiceStudio OBS Connector Relay** | the separate standalone relay for VoiceStudio. SimpleVoice OBS is a replacement for it that also brings its own voices. |
| **OmniVoice**, **Kokoro** | the voice models used by the graphics card and processor engine |

## Build from source

You need Go 1.24 or newer and a POSIX shell (Linux, macOS, or Git Bash on Windows). Node.js 18+ is optional (extension tests).

```
./test.sh     # vet, tests with the race detector, Windows compile check, extension protocol tests
./build.sh    # writes dist/SimpleVoice-OBS-Setup.exe
```

The code uses only the Go standard library and Python for the engine worker. The layout, design decisions and the hard-won
lessons are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and [AGENTS.md](AGENTS.md).
The tests cannot run the real voice models: they use a fake engine. What was verified on real hardware is listed in [CHANGELOG.md](CHANGELOG.md).

## License

GPL-3.0-only, see [LICENSE](LICENSE). Third-party components and their licences: [THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt).
Inspired by Social Stream Ninja. Not affiliated with k2-fsa (OmniVoice), hexgrad (Kokoro) or OpenAI (Whisper).
