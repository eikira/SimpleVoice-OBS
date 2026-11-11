# SimpleVoice OBS engine worker: keeps a voice engine and the transcription model loaded.
# Started and stopped by SimpleVoiceOBSRelay.exe. It only listens on 127.0.0.1 and needs a token.
#
#   --backend omnivoice   NVIDIA GPU, clones voices from a reference recording
#   --backend kokoro      CPU only, built-in voices (no cloning), for computers without a GPU
#
# Copyright (C) 2026 eikira.
# SPDX-License-Identifier: GPL-3.0-or-later
# OmniVoice (k2-fsa): code Apache-2.0, model weights CC-BY-NC (non-commercial).
# Kokoro-82M: Apache-2.0. kokoro-onnx: MIT. Whisper (OpenAI): MIT.
import argparse
import faulthandler
import hmac
import io
import json
import math
import os
import re
import struct
import sys
import threading
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

status = {"state": "loading", "error": "", "reason": "", "device": "", "asr": "loading", "asr_error": "", "asr_device": ""}
speak_lock = threading.Lock()  # one speech at a time
backend = None
transcriber = None
token = os.environ.get("OMNIVOICE_TOKEN", "")
FAKE = os.environ.get("OMNIVOICE_FAKE") == "1"

# Built-in voices of the CPU engine: shown name -> Kokoro voice. Names match the starter voices
# that setup makes for the GPU engine, so a voice name works with either engine.
KOKORO_VOICES = {
    "Heart": "af_heart", "Bella": "af_bella", "Nicole": "af_nicole", "Aoede": "af_aoede", "Kore": "af_kore", "Sarah": "af_sarah",
    "Michael": "am_michael", "Fenrir": "am_fenrir", "Puck": "am_puck",
    "Emma": "bf_emma", "Isabella": "bf_isabella", "George": "bm_george", "Fable": "bm_fable",
}
STARTERS = list(KOKORO_VOICES)  # every processor voice also becomes a sample voice for the graphics card engine
STARTER_TEXT = "Hello, and welcome to the stream. Thanks for being here, I hope you enjoy the show and have a great time."


_ONES = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve",
         "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"]
_TENS = ["", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"]
_SCALES = [(10**9, "billion"), (10**6, "million"), (10**3, "thousand")]
_ORD = {"one": "first", "two": "second", "three": "third", "five": "fifth", "eight": "eighth", "nine": "ninth",
        "twelve": "twelfth"}


def _say_below_1000(n):
    out = []
    if n >= 100:
        out.append(_ONES[n // 100] + " hundred")
        n %= 100
    if n >= 20:
        t = _TENS[n // 10]
        out.append(t + ("-" + _ONES[n % 10] if n % 10 else ""))
    elif n > 0 or not out:
        out.append(_ONES[n])
    return " ".join(out)


def say_number(n):
    """English words for a whole number below one trillion (1234 -> one thousand two hundred thirty-four)."""
    if n < 1000:
        return _say_below_1000(n)
    out = []
    for size, name in _SCALES:
        if n >= size:
            out.append(_say_below_1000(n // size) + " " + name)
            n %= size
    if n:
        out.append(_say_below_1000(n))
    return " ".join(out)


def say_ordinal(n):
    words = say_number(n)
    cut = max(words.rfind(" "), words.rfind("-")) + 1  # the last word decides the ending
    head, last = words[:cut], words[cut:]
    if last in _ORD:
        last = _ORD[last]
    elif last.endswith("y"):
        last = last[:-1] + "ieth"
    else:
        last += "th"
    return head + last


_NUM = re.compile(
    r"(?P<time>\b(?P<h>\d{1,2}):(?P<m>\d{2})\b)"
    r"|(?P<cur>\$(?P<dol>\d{1,3}(?:,\d{3})+|\d+)(?:\.(?P<cents>\d{2}))?)"
    r"|(?P<ord>\b(?P<on>\d+)(?:st|nd|rd|th)\b)"
    r"|(?P<dec>(?P<di>\d{1,3}(?:,\d{3})+|\d+)\.(?P<df>\d+))(?P<pc1>\s*%)?"
    r"|(?P<int>\d{1,3}(?:,\d{3})+|\d+)(?P<pc2>\s*%)?"
)


def _num_sub(m):
    def whole(s):
        s = s.replace(",", "")
        if len(s) > 12:  # a huge digit string stays as it is (and is never turned into a huge integer)
            return None
        n = int(s)
        return say_number(n) if n < 10**12 else None

    if m.group("time"):
        h, mi = int(m.group("h")), int(m.group("m"))
        if h > 23 or mi > 59:
            return m.group(0)
        return say_number(h) + ("" if mi == 0 else " oh " + say_number(mi) if mi < 10 else " " + say_number(mi)) + (" o'clock" if mi == 0 else "")
    if m.group("cur"):
        d = whole(m.group("dol"))
        if d is None:
            return m.group(0)
        n = int(m.group("dol").replace(",", ""))
        out = d + (" dollar" if n == 1 else " dollars")
        if m.group("cents") and int(m.group("cents")):
            c = int(m.group("cents"))
            out += " and " + say_number(c) + (" cent" if c == 1 else " cents")
        return out
    if m.group("ord"):
        n = int(m.group("on")) if len(m.group("on")) <= 12 else 10**12
        return say_ordinal(n) if n < 10**12 else m.group(0)
    if m.group("dec"):
        d = whole(m.group("di"))
        if d is None:
            return m.group(0)
        out = d + " point " + " ".join(_ONES[int(c)] for c in m.group("df"))
        return out + (" percent" if m.group("pc1") else "")
    d = whole(m.group("int"))
    if d is None:
        return m.group(0)
    return d + (" percent" if m.group("pc2") else "")


def spoken_numbers(text):
    """Write the numbers in an English text as words, so the model reads them and plans the right length.
    Text in [brackets] (pronunciation hints, sound tags) is left alone."""
    parts = re.split(r"(\[[^\]]*\])", text)
    return "".join(p if p.startswith("[") else _NUM.sub(_num_sub, p) for p in parts)


def log(msg):
    print(msg, file=sys.stderr, flush=True)


class NoGPU(RuntimeError):
    pass


# ---- speech engines ------------------------------------------------------------------------


class OmniBackend:
    kind = "gpu"

    def __init__(self, model_dir):
        log("loading PyTorch...")
        import torch

        log("loading the OmniVoice code...")
        from omnivoice import OmniVoice

        if not torch.cuda.is_available():
            raise NoGPU("No NVIDIA GPU is available to PyTorch.")
        self.torch = torch
        log("loading the voice model onto the graphics card...")
        self.model = OmniVoice.from_pretrained(model_dir, device_map="cuda:0", dtype=torch.float16)
        self.prompts = {}  # reference voice -> prepared prompt, so each voice is analysed only once
        status["device"] = torch.cuda.get_device_name(0)
        log("warming up (the first sentence is slow)...")
        try:  # first run compiles GPU kernels; do it now, not during the first chat message
            with torch.inference_mode():
                self.model.generate(text="Hello.", num_step=4)
        except Exception as e:  # warm-up is optional
            log("warm-up skipped: %s" % e)

    def voices(self):
        return []

    def synth(self, text, voice, wav, ref_text, speed, steps, language=""):
        key = (wav, os.path.getmtime(wav), ref_text)
        prompt = self.prompts.get(key)
        if prompt is None:
            with self.torch.inference_mode():
                prompt = self.model.create_voice_clone_prompt(ref_audio=wav, ref_text=ref_text)
            if len(self.prompts) >= 16:
                self.prompts.pop(next(iter(self.prompts)))
            self.prompts[key] = prompt
        args = {"text": text, "voice_clone_prompt": prompt, "num_step": steps}
        if language:  # naming the language gives slightly better results than leaving it open
            args["language"] = language
        if abs(speed - 1.0) > 1e-6:
            args["speed"] = speed
        audio = self.model.generate(**args)[0]
        import numpy as np

        pcm = (np.clip(np.asarray(audio, dtype=np.float32), -1.0, 1.0) * 32767).astype("<i2").tobytes()
        return pcm, int(self.model.sampling_rate)


class KokoroBackend:
    kind = "cpu"

    def __init__(self, kokoro_dir):
        from kokoro_onnx import Kokoro

        self.k = Kokoro(os.path.join(kokoro_dir, "kokoro-v1.0.int8.onnx"), os.path.join(kokoro_dir, "voices-v1.0.bin"))
        have = set(self.k.get_voices())
        self.ids = {n.lower(): (n, v) for n, v in KOKORO_VOICES.items() if v in have}
        status["device"] = "CPU"

    def voices(self):
        return [n for n, _ in self.ids.values()]

    def synth(self, text, voice, wav, ref_text, speed, steps, language=""):
        if voice.lower() not in self.ids:
            raise ValueError("The voice '%s' is not one of the built-in voices." % voice)
        name, vid = self.ids[voice.lower()]
        audio, sr = self.k.create(text, voice=vid, speed=speed, lang="en-gb" if vid[0] == "b" else "en-us")
        import numpy as np

        return (np.clip(audio, -1.0, 1.0) * 32767).astype("<i2").tobytes(), int(sr)


class FakeBackend:
    """For tests only (OMNIVOICE_FAKE=1): a tone whose length follows the text and the speed."""

    def __init__(self, kind):
        self.kind = kind
        if kind == "gpu" and os.environ.get("OMNIVOICE_FAKE_NOGPU") == "1":
            raise NoGPU("No NVIDIA GPU is available to PyTorch.")
        if kind == "gpu" and os.environ.get("OMNIVOICE_FAKE_FAIL") == "1":
            raise ImportError("cannot import name 'AutoFeatureExtractor' from 'transformers'")
        if os.environ.get("OMNIVOICE_FAKE_SLOW") == "1":
            time.sleep(1.5)
        status["device"] = "fake"

    def voices(self):
        return list(KOKORO_VOICES)[:4] if self.kind == "cpu" else []

    def synth(self, text, voice, wav, ref_text, speed, steps, language=""):
        log("fake synth asked for speed %.2f, language %r, text %r" % (speed, language, text))
        sr = 24000
        n = int(sr * max(0.2, len(text) * 0.05 / speed))
        return b"".join(struct.pack("<h", int(8000 * math.sin(i * 0.05))) for i in range(n)), sr


def to_wav(pcm, sr):
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(pcm)
    return buf.getvalue()


# ---- transcription (Whisper), loaded at start so it is ready when a recording is imported --------


def split_audio(a, sr, limit=28.0, search=4.0):
    """Whisper reads 30 s at a time. Cut a long recording at its quietest spot near each limit."""
    import numpy as np

    segs, start, size, frame = [], 0, int(limit * sr), int(0.2 * sr)
    while len(a) - start > size:
        lo, hi = start + size - int(search * sr), start + size
        best, cut = None, hi
        for p in range(lo, hi - frame, frame // 4):
            e = float(np.mean(np.square(a[p:p + frame])))
            if best is None or e < best:
                best, cut = e, p + frame // 2
        segs.append(a[start:cut])
        start = cut
    segs.append(a[start:])
    return segs


class Transcriber:
    def __init__(self, asr_dir):
        self.dir = asr_dir
        self.pipe = None
        self.ready = threading.Event()

    def begin(self):
        """Starts loading. This runs after the voice engine has loaded (or failed), never at the same
        time: two threads importing PyTorch and transformers together can break each other's imports."""
        threading.Thread(target=self.load, daemon=True).start()

    def load(self):
        try:
            if FAKE:
                status["asr"], status["asr_device"] = "ready", "fake"
                return
            if not os.path.isdir(self.dir):
                raise RuntimeError("The transcription model is not installed. Run Setup again to add it.")
            import torch
            from transformers import pipeline

            # On a big GPU Whisper lives there too; otherwise it uses the CPU and leaves the video memory to the voices.
            gpu = torch.cuda.is_available() and torch.cuda.get_device_properties(0).total_memory >= 12 * 2**30
            dev = "cuda:0" if gpu else "cpu"
            self.pipe = pipeline("automatic-speech-recognition", model=self.dir, dtype=torch.float16 if gpu else torch.float32, device=dev)
            status["asr"], status["asr_device"] = "ready", "GPU" if gpu else "CPU"
            log("transcription model ready on %s" % status["asr_device"])
        except Exception as e:
            status["asr"], status["asr_error"] = "error", str(e)
            log("transcription model failed to load: %r" % e)
        finally:
            self.ready.set()

    def transcribe(self, wav):
        if not self.ready.wait(600):
            raise RuntimeError("The transcription model is still loading. Try again in a minute.")
        if status["asr"] != "ready":
            raise RuntimeError(status["asr_error"] or "The transcription model is not available.")
        if FAKE:
            return "This is a fake transcript."
        import numpy as np
        import torch
        import torchaudio

        with wave.open(wav, "rb") as w:
            sr, ch, raw = w.getframerate(), w.getnchannels(), w.readframes(w.getnframes())
        a = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
        if ch > 1:
            a = a.reshape(-1, ch).mean(axis=1)
        if sr != 16000:
            a = torchaudio.functional.resample(torch.from_numpy(a), sr, 16000).numpy()
        out = []
        for seg in split_audio(a, 16000):
            if len(seg) > 1600:
                out.append(self.pipe({"array": seg, "sampling_rate": 16000})["text"].strip())
        return " ".join(t for t in out if t)


# ---- HTTP ------------------------------------------------------------------------------------


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, code, body, ctype="application/json"):
        if not isinstance(body, bytes):
            body = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def allowed(self):
        if token and hmac.compare_digest(self.headers.get("X-Worker-Token", ""), token):
            return True
        self.reply(403, {"error": "forbidden"})
        return False

    def body(self):
        n = int(self.headers.get("Content-Length", "0"))
        if n <= 0 or n > 65536:
            raise ValueError("bad request size")
        return json.loads(self.rfile.read(n))

    def do_GET(self):
        if not self.allowed():
            return
        if self.path == "/health":
            self.reply(200, status)
        elif self.path == "/voices":
            self.reply(200, {"voices": backend.voices() if backend else []})

    def do_POST(self):
        if not self.allowed():
            return
        try:
            req = self.body()
            if self.path == "/speak":
                text, voice = str(req["text"]), str(req.get("voice", ""))
                wav, ref_text = str(req.get("wav", "")), str(req.get("ref_text", ""))
                speed, steps = float(req.get("speed", 1.0)), int(req.get("steps", 16))
                language = str(req.get("language", ""))[:20]
                if status["state"] != "ready":
                    raise RuntimeError("engine is not ready")
                if req.get("numbers") and backend.kind == "gpu" and language in ("", "en", "English"):
                    text = spoken_numbers(text)
                with speak_lock:
                    t = time.time()
                    pcm, sr = backend.synth(text, voice, wav, ref_text, speed, steps, language)
                log("speech: %d characters, %.1f s audio, %.2f s to make (speed %.2f, %d steps, language %s)" % (len(text), len(pcm) / 2 / sr, time.time() - t, speed, steps, language or "automatic"))
                self.reply(200, to_wav(pcm, sr), "audio/wav")
            elif self.path == "/transcribe":
                t = time.time()
                text = transcriber.transcribe(str(req["wav"])).strip()  # no speech lock: it must not wait for speech
                log("transcribed in %.1f s" % (time.time() - t))
                self.reply(200, {"text": text})
            else:
                self.reply(404, {"error": "not found"})
        except Exception as e:
            log("%s failed: %r" % (self.path.strip("/"), e))
            self.reply(500, {"error": str(e)})


class Server(ThreadingHTTPServer):
    """While libraries are loading, requests are answered on this thread instead of a new one.
    Windows can deadlock a new thread against a library that is being loaded (seen on a real PC,
    where the relay's first status checks froze PyTorch's import for good)."""

    def busy_loading(self):
        return status["state"] == "loading" or status["asr"] == "loading"

    def process_request(self, request, client_address):
        if self.busy_loading():
            self.process_request_thread(request, client_address)
        else:
            super().process_request(request, client_address)


def watch_parent(pid):
    """Quit when the relay is gone, so no engine is left running without it."""
    if os.name == "nt":
        # Wait for the relay process itself. Never read stdin here: a thread blocked in a read on a
        # pipe froze the loading of NumPy on Windows (another thread waited for it for good).
        try:
            import ctypes

            k32 = ctypes.windll.kernel32
            k32.OpenProcess.restype = ctypes.c_void_p
            k32.WaitForSingleObject.argtypes = [ctypes.c_void_p, ctypes.c_uint32]
            handle = k32.OpenProcess(0x00100000, False, pid)  # SYNCHRONIZE
            if not handle:
                log("could not watch the relay process (error %d)" % k32.GetLastError())
                return
            k32.WaitForSingleObject(handle, 0xFFFFFFFF)
        except Exception as e:
            log("could not watch the relay process: %r" % e)
            return
        os._exit(0)
        return
    # Elsewhere the relay holds our stdin open. When it closes (the program ended), so do we.
    try:
        sys.stdin.buffer.read()
    finally:
        os._exit(0)


def make_backend(kind, a):
    if FAKE:
        return FakeBackend(kind)
    return OmniBackend(a.model) if kind == "gpu" else KokoroBackend(a.kokoro)


def load(a):
    global backend
    # If loading takes long, print where every thread is stuck, so the engine log shows the reason.
    wait = float(os.environ.get("OMNIVOICE_DUMP_AFTER", "45"))
    log("worker started (Python %s, process %d)" % (sys.version.split()[0], os.getpid()))
    faulthandler.dump_traceback_later(wait, repeat=True, file=sys.stderr)
    try:
        backend = make_backend("gpu" if a.backend == "omnivoice" else "cpu", a)
        status["state"] = "ready"
        log("engine ready on %s" % status["device"])
    except Exception as e:
        status["state"], status["error"] = "error", str(e)
        status["reason"] = "no_gpu" if isinstance(e, NoGPU) else "error"
        log("engine failed to start: %r" % e)
    finally:
        faulthandler.cancel_dump_traceback_later()
        transcriber.begin()


def make_starters(a):
    """Setup runs this once: the CPU engine reads a short text in four voices. They become the
    starter voices of the GPU engine (recording and exact words, free of any licence worries)."""
    os.makedirs(a.make_starters, exist_ok=True)
    b = KokoroBackend(a.kokoro)
    only = {n.strip().lower() for n in a.only.split(",") if n.strip()}
    for name in STARTERS:
        if name.lower() not in b.ids or (only and name.lower() not in only):
            continue
        wav = os.path.join(a.make_starters, name + ".wav")
        if os.path.exists(wav) and os.path.exists(os.path.join(a.make_starters, name + ".txt")):
            continue
        pcm, sr = b.synth(STARTER_TEXT, name, "", "", 1.0, 0)
        with open(wav, "wb") as f:
            f.write(to_wav(pcm, sr))
        with open(os.path.join(a.make_starters, name + ".txt"), "w", encoding="utf-8") as f:
            f.write(STARTER_TEXT + "\n")
        print("starter voice made: " + name, flush=True)


def main():
    global transcriber
    ap = argparse.ArgumentParser()
    ap.add_argument("--parent", type=int, default=0)
    ap.add_argument("--port", type=int)
    ap.add_argument("--backend", choices=["omnivoice", "kokoro"], default="omnivoice")
    ap.add_argument("--model", default="")
    ap.add_argument("--kokoro", default="")
    ap.add_argument("--asr", default="")
    ap.add_argument("--make-starters", default="")
    ap.add_argument("--only", default="")  # with --make-starters: comma separated names, default all
    a = ap.parse_args()
    if a.make_starters:
        return make_starters(a)
    if not a.port:
        ap.error("--port is required")
    threading.Thread(target=watch_parent, args=(a.parent,), daemon=True).start()
    server = Server(("127.0.0.1", a.port), Handler)
    transcriber = Transcriber(a.asr)
    threading.Thread(target=load, args=(a,), daemon=True).start()
    server.serve_forever()


if __name__ == "__main__":
    main()
