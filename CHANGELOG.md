# Changelog

Version numbers: the first production release is 1.0.0. Updates go up by 0.0.1 (1.0.1, 1.0.2, ... 1.0.9), then 1.1.0.
Each entry says what changed and what is **not verified**.

## 1.0.4

Works with extension 0.3.1 (soundbites need it), and 0.3.0 / 0.2.0 for everything else. Installers for Windows, Linux and macOS (Apple Silicon).

- **New (beta): Soundbites.** Control panel section "Soundbites (beta)". Add a sound file under an emote name (for example `choppa` for `:choppa:`) with a volume; Play and Remove for each. A message that contains `:choppa:` plays the clip. A message with clips is answered as ONE audio file: every spoken piece is made first, then pieces and clips are joined with short gaps, so the order is always as written. A message of only clips needs no valid voice. An unknown `:name:` is spoken as text as before.
- **Soundbites keep their format:** a clip keeps its sample rate (up to 48 kHz) and its channels (mono or stereo; more than 2 channels become stereo). A message of only clips plays in the clip's own format. In a message with speech and a stereo clip the file is stereo: the speech is repeated in both channels, the clip keeps left and right.
- Limits: a clip is at most 15 seconds, at most 6 clips are played from one message (further ones are dropped), at most 30 seconds of clips per message, at most 300 clips. Names: letters, digits, `-` and `_`, up to 40 characters, not case sensitive. Clips are stored in the program's data folder (`soundbites/`; Windows `%LOCALAPPDATA%\SimpleVoiceOBS`, Linux `~/.config/SimpleVoiceOBS`). An update keeps them. New address `/v1/soundbites` lists the clip names (the extension uses it).
- **New: Setup choice "Soundbites only":** untick every engine (or click "Soundbites only (smallest)") to install just the relay, Python and ffmpeg: about 0.1 GB download, 0.25 GB on disk. Sound clips play; text is not read aloud. The control panel says "Soundbites only" until an engine is added by running Setup again. The licence box is shown only when the voice model is ticked.
- **New: Linux Setup** (64 bit Intel/AMD): install folder `~/.local/share/SimpleVoice-OBS`, menu entry, desktop icon, start at login. The network page and the control panel show a firewall hint on Linux.
- **New: macOS Setup** (Apple Silicon): standalone Python, PyTorch with the Apple GPU (MPS), launcher app in `~/Applications`, start at login.
- **GTX 16 series:** Setup, the component list and the README say that these cards work but are very slow and not recommended. Setup marks such a card as "low".
- **Fixed: the Copy button on the "Listening address" page** copied nothing when the page was opened over a network address (the browser only allows its copy function on secure pages). It now uses a fallback, shows "Copied" only when it worked, otherwise selects the address and shows "Press Ctrl+C".
- **Setup:** Copy log button (for error reports).
- **Not verified:** Windows run of this exact build; Linux Setup on a real Linux computer (never run on one); **macOS is mostly untested** (the owner's install and test voices worked; voice cloning, the extension with the relay and the desktop shortcut were not tried); soundbites with real voices in OBS and how OBS plays a 2-channel file; what Uninstall does with the clips; Rumble messages with several emotes; the Copy button in a real browser.

## 1.0.3

- **Firewall question:** when the relay starts on a network address, it makes one short connection attempt from that address to the gateway of that network (port 80; the answer does not matter, and nothing goes to the internet). A firewall such as Windows Firewall or simplewall only asks about a program when it sees its traffic; listening alone shows nothing. The log says what happened ("Firewall check: ..."). Never done in "this PC only" mode. The page `/network` says that the firewall may ask and to allow it for the private network. Same change as in the standalone relay 0.1.0.
- **Not verified:** that Windows Firewall and simplewall really show their question because of this attempt (tested here that the attempt leaves from the chosen address and that the gateway is found on Linux; the Windows gateway lookup compiles but was not run on Windows).

## 1.0.2

Works with extension 0.2.0 and newer.

- **Listening address:** choose which network address the relay answers on, for OBS or the extension on another computer. The page `/network` (only this computer can open it) lists the addresses of this computer with the adapter name of each. Private ranges (192.168.x.x, 10.x.x.x, 172.16-31.x.x) are offered; adapters that look like a VPN or a virtual adapter, and the 100.64.x.x range, are marked in red; other (not private) addresses are hidden in a section with a red warning and a confirmation box. Two prominent notices: choose your real network, not a VPN; and the extension's Detect looks on one port only, so the port must match. Open it from the tray menu "Listening address (network)...", from the control panel, or with `--open-network`. Without a browser: `--list-addresses` and `--set-host ADDRESS` (`--yes-not-private` confirms a not-private address). Saving restarts the relay. If the saved address is gone (the network changed), the relay starts on this PC only and says why.
- **The control panel stays on this computer.** The panel page, `/api/*` and the shutdown call answer only this computer (loopback or one of its own addresses), also when the relay listens on a network address. Only the extension calls (`/v1/...`) and the OBS source are open to the network.
- **Firefox:** the relay now also accepts calls from the Firefox version of the extension (`moz-extension://` origin).
- `GET /info` also reports `name`, `listening` (the address it listens on) and `adapter`, so the extension can recognise this relay when it searches the network and show where it listens.
- **Not verified:** Windows tray entry and restart on Windows (built and `go vet` checked only); macOS and Linux builds (this relay is Windows-only in practice); real LAN and VPN adapters (tested with simulated lists and on a machine with one network card); Firefox.

## 1.0.1

Fixes and small changes. Works with extension 0.2.0 (which has the SimpleVoice OBS choice and reads the new `min_speed`) and still with 0.1.1.

- **Setup closes properly.** Closing the Setup window used to leave `SimpleVoice-OBS-Setup.exe` (and the browser window's processes) running in Task Manager when the window could not be watched. Now the page says goodbye when it closes (and asks every second while it is open); setup then ends, and ends the app window's helper processes so the temporary profile folder is removed. Closing the window still stops and reverses a running install. A page that only falls silent (a sleeping computer, a hidden window) never cancels an install: setup finishes it and then closes.
- **Setup ends earlier Setup runs.** When `SimpleVoice-OBS-Setup.exe` starts, other running copies of it, their leftover browser windows and old temporary profile folders are ended. Only under the real program name (a test build or a renamed copy never ends anything).
- **Slowest speed per voice.** `GET /v1/audio/voices` now has a `min_speed` number for every voice: 0.8 for imported voices, 0.5 for the sample voices and the built-in voices. It is what speech already did; the extension can now limit its speed choices to it.
- **Last word no longer swallowed.** On the graphics card engine a text that ends in a letter or a digit gets a comma at the end ("...stream ever" was spoken as "...stream e"). Texts that already end in punctuation or an emoji are left as they are. The processor engine is untouched.
- Texts that told you to set *VoiceStudio address* to the SimpleVoice OBS address now say: choose SimpleVoice OBS in the extension (0.2.0 or newer).

**Not verified (needs real Windows):** closing the Setup window with Edge and with Chrome (the app window is the part that cannot be tested here); that no `SimpleVoice-OBS-Setup.exe` or browser process is left in Task Manager; the PowerShell commands that end other Setup runs and window processes (written carefully, not run here); the comma fixing the swallowed word with the real OmniVoice model (the fake worker only shows the comma is sent).

## 1.0.0

First production release of SimpleVoice OBS (earlier test builds 0.1 to 0.15 were called OmniVoice Relay).

### What it is
- One Windows installer (Setup) that installs the SimpleVoice OBS Relay, Python, the voice engines and models, shortcuts, and an uninstaller.
- Graphics card engine (OmniVoice, voice cloning) and processor engine (Kokoro, built-in voices).
- Control panel in the browser: voices (import, rename, edit words, delete), automatic transcription (Whisper), Test with
  Generate / Play-Stop / Save (MP3, WAV, FLAC, AAC), voice quality, language, numbers as words.
- Compatible with the VoiceStudio+SimpleVoice OBS Connector extension and the OBS Browser Source.
- Setup: component list with sizes, 13 selectable sample voices, no-NVIDIA-card path (processor engine only), an inventory file
  so updates remove files that a newer version no longer ships.

### Decisions worth knowing
- The graphics card engine needs a GTX 16 series, RTX 20 series or newer card. The bundled PyTorch (2.8.0, CUDA 12.8) has no code for older cards.
- Imported voices cannot go slower than speed 0.8 (the model repeats parts of the recording at 0.7 and below). Sample voices and built-in voices go to 0.5.
- Recordings longer than 20 seconds are refused (3 to 10 seconds works best).
- Updates are manual. The relay never contacts the internet by itself.

### Verified on real hardware (by the owner, on test builds before 1.0.0)
- The graphics card engine loads and speaks on an NVIDIA RTX 3050 (4 GB). 4 GB is below the 6 GB recommendation.
- Voice import with cloning, the processor engine, speed changes on the processor engine, the uninstaller.

### Not verified
- 1.0.0 itself on a clean PC, and an upgrade from a test build (1.0.0 does not touch test builds: uninstall them first).
- Cards with 6 GB or more, and the quality of the 32-step default on every voice.
- The new panel (Generate, Play/Stop, Save), language and numbers-as-words on real speech.
- The no-NVIDIA-card install path on a PC without such a card, and the Windows tray tooltip and shortcuts.
- Offline operation after install, very long texts, Windows free-space reading, downloads behind a strict firewall.
