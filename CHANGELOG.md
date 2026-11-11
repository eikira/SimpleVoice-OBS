# Changelog

Version numbers: the first production release is 1.0.0. Updates go up by 0.0.1 (1.0.1, 1.0.2, ... 1.0.9), then 1.1.0.
Each entry says what changed and what is **not verified**.

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
