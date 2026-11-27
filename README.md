# SimpleVoice OBS

Read your stream chat aloud in a cloned voice, on your own computer, with OBS. Free and open source (GPL-3.0).

SimpleVoice OBS is one installer that sets up everything: a small background program (the **SimpleVoice OBS Relay**), a
voice engine, and a control panel in your browser where you add voices and test them. It works together with the
**VoiceStudio+SimpleVoice OBS Connector** Chrome or Firefox extension and the OBS Browser Source, exactly as the VoiceStudio setup
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
- **Private.** Everything runs on your PC. The relay listens on `127.0.0.1` (this PC only) unless you choose a network address for OBS or the extension on another computer (see below). The internet is used while installing (see below).

## Requirements

| | |
|---|---|
| System | Windows 10 or 11 (64 bit); Linux (64 bit Intel/AMD, with a desktop and a web browser); macOS on Apple Silicon (M1 or newer; **mostly untested**, see below) |
| Voice cloning | NVIDIA RTX 20 series or newer, with 6 GB video memory or more (GTX 16 series works, but is very slow and not recommended); on a Mac the Apple GPU, 16 GB of memory or more recommended |
| Any other PC | The processor engine is installed instead: built-in voices, no cloning, no transcription |
| Disk space | about 11.4 GB for everything, about 0.5 GB for the processor engine only, about 0.25 GB for soundbites only (estimates) |
| Download | about 8.3 GB for everything, about 0.26 GB for the processor engine only, about 0.1 GB for soundbites only (estimates) |

Older NVIDIA cards (GTX 10 series and before) and AMD or Intel graphics cards cannot use the graphics card engine. Setup detects this and installs only the processor engine, without downloading anything that only the graphics card engine needs.

## Install

1. Download the zip for your system from the releases page and unzip it: `SimpleVoice-OBS-Setup-<version>-Windows.zip`, `-Linux.zip` or `-macOS.zip`.
2. Start the Setup (Windows: double-click `SimpleVoice-OBS-Setup.exe`; Linux and macOS: make the file executable with `chmod +x` and run it), tick what you want (**Soundbites only (smallest)** unticks every voice engine: only sound clips for emotes will play, no speech), accept the licence note if the voice model is ticked, click **Install**. The size shown updates with what you tick.
3. When it finishes, the control panel opens. Add a voice and press **Generate** in the Test section.
4. In the **VoiceStudio+SimpleVoice OBS Connector** extension (0.3.1 recommended) open Settings, section 2, choose **SimpleVoice OBS** (or click **Detect**), then click **Load voices** in section 3. Keep your OBS Browser Source URL as it is.

Setup needs no administrator rights and does not send anything anywhere. Where it installs:

| System | Program and engine | Extras |
|---|---|---|
| Windows | `%LOCALAPPDATA%\SimpleVoice-OBS` | Start menu entry, optional desktop icon and start with Windows. If Windows or your firewall asks, click Allow for the program and for `python.exe`. |
| Linux | `~/.local/share/SimpleVoice-OBS` | Menu entry, desktop icon, start at login. If another computer cannot reach the relay, allow its port in the firewall (the control panel and the network page show the command). |
| macOS | `~/Applications/SimpleVoice OBS.app` | Launcher app, desktop shortcut where possible, start at login. macOS may refuse an unsigned program at first (right-click, Open). |

During install, Setup downloads from python.org (Linux and macOS: github.com), pypi.org, files.pythonhosted.org, download.pytorch.org (Windows), huggingface.co and github.com. The relay is designed to work offline after install (not verified on every setup).

**macOS is mostly untested.** The owner installed it on an Apple Silicon Mac and the test voices worked. Not tested: voice cloning, the extension together with the relay, the desktop shortcut, a second Mac, and Intel Macs (not supported).

## Soundbites (beta)

New in 1.0.4. Soundbites work in the author's tests but have had little real-world use, so expect rough edges and tell us what breaks. They do not change how your voices and normal speech work.

Play a sound clip for an emote written like `:choppa:` (Rumble style). In the control panel, section *Soundbites (beta)*, give the emote name (without colons), choose a sound file (up to 15 seconds) and set a volume. A chat message with `:choppa:` plays the clip; text before and after it is spoken in the written order (speech, clip, speech) as one piece. A stereo clip stays stereo. Needs extension 0.3.1.

## Another computer: listening address

By default only this PC can reach the relay. To use OBS or the extension on another computer, open the **network page** (link in the control panel, or tray menu *Listening address (network)...*). The page, which opens only on this PC:

- lists the addresses of this PC with the **adapter name** of each; private ranges are offered, VPN-looking adapters and 100.64.x.x are marked red,
  other addresses need a confirmation;
- says: **choose your real home or office network, not a VPN adapter**, and **the extension's Detect looks on one port only, so the port must match**;
- shows the address to enter in the extension, with a Copy button.

Saving restarts the relay. After the restart it makes one short connection attempt to the gateway of that network so that Windows Firewall or
simplewall can ask about the program; allow it for your private network. Anyone on that network who knows your OBS ID can send speech: use it only on
a network you trust. The control panel and the `/api/` calls stay reachable from this PC only.

## Using it

- Tray icon: double-click opens the control panel. The menu has: control panel, voices folder, log, change port, listening address (network), exit.
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
| **VoiceStudio+SimpleVoice OBS Connector** | the Chrome extension, version 0.2.0 or newer (before 0.2.0 it was called "VoiceStudio OBS Connector" and worked with VoiceStudio only) |
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
