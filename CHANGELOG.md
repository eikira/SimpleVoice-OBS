# Changelog

Version numbers: the first production release is 1.0.0. Updates go up by 0.0.1 (1.0.1, 1.0.2, ... 1.0.9), then 1.1.0.
Each entry says what changed and what is **not verified**.

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
