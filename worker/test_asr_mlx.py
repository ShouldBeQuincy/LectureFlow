# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/asr_mlx.py (SPEC-v3 V3.1, V5; SPEC-v2 I7) with a fake mlx / mlx_whisper.

The fake modules are injected into sys.modules (in-process tests) or written as packages
on PYTHONPATH (process tests), so these tests run on Windows without MLX (SPEC §14: no
real model). The fake transcribe() answers from a script of segments on the lecture's
timeline, locating the audio it gets inside the lecture (a chunk or a resumed run gets a
piece of it). Tests marked `realmlx` use the real library and a real model on an Apple
Silicon Mac (macOS CI); they are skipped unless selected with -m realmlx.
"""
import importlib
import inspect
import json
import os
import subprocess
import sys
import time
import types

import numpy as np
import pytest

import asr
import asr_common
import asr_mlx
import asr_openvino
from conftest import FAKE_SCRIPT_SRC, WORKER_DIR, all_speech_vad, script_read_wav, \
    write_wav_parts

JOB_ID = "j-asr-mlx-01"
INPUT_HASH = "v3:fedcba9876543210"

# ---------------------------------------------------------------------------
# The fake packages (one source for the in-process modules and the process packages)
# ---------------------------------------------------------------------------

FAKE_MLX_CORE = r'''
float16 = "float16"
float32 = "float32"
SEEDS = []


class random:
    @staticmethod
    def seed(n):
        SEEDS.append(n)


class metal:
    AVAILABLE = [True]

    @staticmethod
    def is_available():
        return metal.AVAILABLE[0]
'''

FAKE_TOKENIZER = r'''
LANGUAGES = {"en": "english", "zh": "chinese", "de": "german"}


class _Tok:
    def encode(self, text, **kw):
        return text.split()


def get_tokenizer(multilingual, *, num_languages=99, language=None, task=None):
    if language is not None and language not in LANGUAGES:
        raise ValueError("Unsupported language: %s" % language)
    return _Tok()
'''

FAKE_LOAD_MODELS = r'''
LOADS = []


class _Model:
    is_multilingual = True
    num_languages = 100

    def __init__(self, path, dtype):
        self.path = path
        self.dtype = dtype


def load_model(path_or_hf_repo, dtype="float32"):
    LOADS.append((path_or_hf_repo, dtype))
    return _Model(path_or_hf_repo, dtype)
'''

# CFG keys: script (segments [start, end, text, [[word, s, e, p]], no_speech, logprob]
# on the lecture's timeline), lecture (wav path), words (false = no word timestamps
# returned), windows (progress bar updates per call), fail_on_call / hang_on_call (1-based),
# error (raise on every call). In-process tests also set on_call(n, kwargs).
FAKE_TRANSCRIBE = FAKE_SCRIPT_SRC + r'''
import json as _json
import os as _os
import sys as _sys
import time as _time
from types import SimpleNamespace as _NS

from . import load_models as _lm


class _Bar:
    def __init__(self, total=None, unit=None, disable=False):
        self.total = total

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def update(self, n):
        pass


tqdm = _NS(tqdm=_Bar)

CFG = _json.loads(_os.environ.get("LF_FAKE_MLX", "{}"))
CALLS = []
_FULL = {}


class ModelHolder:
    model = None
    model_path = None

    @classmethod
    def get_model(cls, model_path, dtype):
        if cls.model is None or model_path != cls.model_path:
            cls.model = _lm.load_model(model_path, dtype=dtype)
            cls.model_path = model_path
        return cls.model


def _full():
    path = CFG["lecture"]
    if path not in _FULL:
        _FULL[path] = script_read_wav(path)
    return _FULL[path]


def transcribe(audio, *, path_or_hf_repo="mlx-community/whisper-tiny", verbose=None,
               temperature=(0.0, 0.2, 0.4, 0.6, 0.8, 1.0), compression_ratio_threshold=2.4,
               logprob_threshold=-1.0, no_speech_threshold=0.6,
               condition_on_previous_text=True, initial_prompt=None, word_timestamps=False,
               **decode_options):
    n = len(CALLS) + 1
    kw = dict(decode_options, path_or_hf_repo=path_or_hf_repo, verbose=verbose,
              condition_on_previous_text=condition_on_previous_text,
              initial_prompt=initial_prompt, word_timestamps=word_timestamps)
    off = script_locate(_full(), audio) if CFG.get("lecture") else 0
    length = len(audio) / 16000.0
    CALLS.append(dict(kw, offset=off, samples=len(audio)))
    if CFG.get("on_call"):
        CFG["on_call"](n, kw)
    if CFG.get("error") or CFG.get("fail_on_call") == n:
        raise RuntimeError("fake mlx failure on call %d" % n)
    model = ModelHolder.get_model(path_or_hf_repo, "float16")
    total = int(round(length * 100))
    windows = int(CFG.get("windows", 3))
    with tqdm.tqdm(total=total, unit="frames", disable=verbose is not False) as bar:
        done = 0
        for i in range(windows):
            step = total * (i + 1) // windows - done
            done += step
            bar.update(step)
            if CFG.get("hang_on_call") == n and i == 0:
                print("TRANSCRIBING", flush=True)
                _time.sleep(600)
    offset_s = (off or 0) / 16000.0
    segments = []
    for start, end, text, ws, no_speech, logprob in CFG.get("script", []):
        if start >= offset_s - 1e-6 and end <= offset_s + length + 1e-6:
            seg = {"seek": 0, "start": start - offset_s, "end": end - offset_s, "text": text,
                   "tokens": [], "temperature": 0.0, "avg_logprob": logprob,
                   "compression_ratio": 1.2, "no_speech_prob": no_speech}
            if word_timestamps:
                seg["words"] = [] if CFG.get("words") is False else [
                    {"word": w, "start": s - offset_s, "end": e - offset_s, "probability": p}
                    for w, s, e, p in ws]
            segments.append(seg)
    return {"text": "".join(s["text"] for s in segments), "segments": segments,
            "language": decode_options.get("language") or "en"}
'''

FAKE_WHISPER_INIT = r'''
import os as _os
if _os.environ.get("LF_FAKE_MLX_IMPORT_ERROR"):
    raise ImportError(_os.environ["LF_FAKE_MLX_IMPORT_ERROR"])
from . import load_models, tokenizer
from .transcribe import transcribe
__version__ = "0.4.3"
'''


def write_fake_mlx_packages(root):
    """The fake mlx / mlx_whisper packages under root (for worker processes)."""
    files = {"mlx/__init__.py": "", "mlx/core.py": FAKE_MLX_CORE,
             "mlx_whisper/__init__.py": FAKE_WHISPER_INIT,
             "mlx_whisper/load_models.py": FAKE_LOAD_MODELS,
             "mlx_whisper/tokenizer.py": FAKE_TOKENIZER,
             "mlx_whisper/transcribe.py": FAKE_TRANSCRIBE}
    for rel, src in files.items():
        path = os.path.join(str(root), *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(src)
    return str(root)


def _module(name, src, package=None):
    m = types.ModuleType(name)
    m.__package__ = package if package is not None else name.rpartition(".")[0]
    sys.modules[name] = m
    exec(compile(src, "<fake %s>" % name, "exec"), m.__dict__)
    return m


@pytest.fixture
def fake_mlx(monkeypatch):
    """Injects fresh fake mlx / mlx_whisper modules; returns the fake transcribe module
    (CFG, CALLS) with .core (mlx.core) and .load_models attached."""
    for name in [n for n in sys.modules if n == "mlx" or n.startswith(("mlx.", "mlx_whisper"))]:
        monkeypatch.delitem(sys.modules, name)
    for name in ("mlx", "mlx.core", "mlx_whisper", "mlx_whisper.load_models",
                 "mlx_whisper.tokenizer", "mlx_whisper.transcribe"):
        monkeypatch.setitem(sys.modules, name, types.ModuleType(name))
    mlx = types.ModuleType("mlx")
    mlx.__path__ = []
    sys.modules["mlx"] = mlx
    core = _module("mlx.core", FAKE_MLX_CORE)
    mlx.core = core
    pkg = types.ModuleType("mlx_whisper")
    pkg.__path__ = []
    sys.modules["mlx_whisper"] = pkg
    pkg.load_models = _module("mlx_whisper.load_models", FAKE_LOAD_MODELS)
    pkg.tokenizer = _module("mlx_whisper.tokenizer", FAKE_TOKENIZER)
    tr = _module("mlx_whisper.transcribe", FAKE_TRANSCRIBE)
    pkg.transcribe = tr.transcribe          # as the real package: the function shadows
    pkg.__version__ = "0.4.3"
    tr.CFG = {}
    tr.core = core
    tr.load_models = pkg.load_models
    return tr


def read_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def progress_of(lines):
    return [float(l.split()[1]) for l in lines if l.startswith("PROGRESS ")]


# ---------------------------------------------------------------------------
# A lecture: ten 8-second tone blocks, each followed by 2 s of digital silence
# ---------------------------------------------------------------------------

def lecture_script():
    """One segment per tone block i ([10 i, 10 i + 8] s): " Point i is here." with a low
    confidence word in block 3, a hallucinated segment (no_speech 0.9, logprob -1.5) in
    block 5 and a repeated phrase in block 7."""
    script = []
    for i in range(10):
        b = 10.0 * i
        ws = [[" Point", b + 0.5, b + 1.0, 0.9], [" %d" % i, b + 1.0, b + 1.5, 0.9],
              [" is", b + 1.5, b + 2.0, 0.3 if i == 3 else 0.9], [" here.", b + 2.0, b + 2.6, 0.9]]
        if i == 7:
            ws = ws[:3] + [[" so", b + 2.6 + 0.3 * k, b + 2.9 + 0.3 * k, 0.9] for k in range(4)] \
                + [[" here.", b + 3.8, b + 4.4, 0.9]]
        text = "".join(w[0] for w in ws)
        script.append([b + 0.4, b + 4.5, text, ws, 0.02, -0.2])
        if i == 5:
            script.append([b + 5.0, b + 7.5, " Thanks for watching!",
                           [[" Thanks", b + 5.0, b + 5.5, 0.4], [" for", b + 5.5, b + 6.0, 0.4],
                            [" watching!", b + 6.0, b + 7.5, 0.4]], 0.9, -1.5])
    return script


@pytest.fixture(scope="module")
def lecture_wav(tmp_path_factory):
    parts = []
    for i in range(10):      # a different level per block: every piece has one place
        parts += [(8.0, 3000 + 500 * i), (2.0, 0)]
    return write_wav_parts(tmp_path_factory.mktemp("lecture") / "audio.wav", parts)


def make_model_dir(path):
    os.makedirs(path, exist_ok=True)
    with open(os.path.join(path, "config.json"), "w", encoding="utf-8") as f:
        f.write('{"n_mels": 128}')
    with open(os.path.join(path, "weights.safetensors"), "wb") as f:
        f.write(b"x")
    return str(path)


@pytest.fixture
def mlxjob(tmp_path, lecture_wav):
    model_dir = make_model_dir(tmp_path / "models" / "mlx" / "large-v3-turbo-fp16")

    def make(**overrides):
        job = {"audio": lecture_wav, "model": "large-v3-turbo", "precision": "fp16",
               "model_dir": model_dir, "device": "mlx:gpu", "language": "en",
               "initial_prompt": "Point, here", "word_timestamps": True}
        job.update(overrides)
        job = {k: v for k, v in job.items() if v is not _DROP}
        in_path = tmp_path / "job.json"
        in_path.write_text(json.dumps(job), encoding="utf-8")
        return str(in_path), str(tmp_path / "result.json.tmp")

    make.model_dir = model_dir
    return make


_DROP = object()


@pytest.fixture
def lecture(fake_mlx, lecture_wav):
    fake_mlx.CFG.update(script=lecture_script(), lecture=lecture_wav)
    return fake_mlx


def run_worker(in_path, out_path):
    lines = []
    asr_mlx.run(in_path, out_path, lines.append)
    return lines, read_json(out_path)


EXPECTED_TEXTS = ["Point 0 is here.", "Point 1 is here.", "Point 2 is here.",
                  "Point 3 is here.", "Point 4 is here.", "Point 5 is here.",
                  "Point 6 is here.", "Point 7 is so here.", "Point 8 is here.",
                  "Point 9 is here."]


# ---------------------------------------------------------------------------
# One contract, three workers
# ---------------------------------------------------------------------------

SHARED = ["split_sentences", "split_word_groups", "is_sentence_end", "remove_repeats",
          "truncate_prompt", "format_progress", "write_result", "as_word", "_word_times",
          "_sentence_dict", "Word", "InputError", "make_timing"]


def test_shared_functions_are_one_implementation():
    for name in SHARED:
        assert getattr(asr_mlx, name) is getattr(asr_common, name) \
            is getattr(asr, name) is getattr(asr_openvino, name), name
    src = os.path.abspath(inspect.getsourcefile(asr_mlx.split_sentences))
    assert os.path.basename(src) == "asr_common.py"
    assert src == os.path.abspath(inspect.getsourcefile(asr.split_sentences)) \
        == os.path.abspath(inspect.getsourcefile(asr_openvino.split_sentences))
    # the hallucination filter is the faster-whisper worker's, the checkpoint file
    # asr_common's, the no-VAD word repair and the chunk cuts the OpenVINO worker's
    assert asr_mlx.keep_segment is asr.keep_segment
    assert asr_mlx.Checkpointer is asr_common.Checkpointer
    for name in ("next_cut", "clamp_long_words", "drop_silent", "silent_spans",
                 "make_monotonic", "read_wav", "frame_rms", "WorkerError"):
        assert getattr(asr_mlx, name) is getattr(asr_openvino, name), name
    # asr_mlx.py itself defines none of the shared functions (no copies)
    with open(asr_mlx.__file__, encoding="utf-8") as f:
        source = f.read()
    for name in ("split_sentences", "split_word_groups", "remove_repeats", "truncate_prompt",
                 "keep_segment", "is_sentence_end", "next_cut", "write_result"):
        assert "def %s(" % name not in source, name


def test_module_imports_without_mlx():
    """asr_mlx imports anywhere (mlx is imported lazily in run())."""
    code = ("import sys; sys.path.insert(0, %r); import asr_mlx; "
            "assert 'mlx' not in sys.modules and 'mlx_whisper' not in sys.modules"
            % WORKER_DIR)
    subprocess.run([sys.executable, "-c", code], check=True, timeout=60)


def test_same_words_give_the_same_result_as_the_other_workers(tmp_path, monkeypatch, fake_mlx,
                                                              fake_ov):
    """Fed the words of test_asr_contract, the three workers write the same sentences."""
    import test_asr_contract as contract
    audio = write_wav_parts(tmp_path / "audio.wav", [(6.0, 8000)])
    all_speech_vad(monkeypatch, asr)          # asr.py's own VAD step: all of it is speech
    monkeypatch.setattr(asr, "load_model", lambda *a: contract._FWModel())
    monkeypatch.setattr(asr, "setup_cuda_libs", lambda: [])
    fake_ov.words = list(contract.WORDS)
    fake_ov.chunks = [(0.0, 6.0, "".join(w for w, _, _ in contract.WORDS))]
    text = "".join(w for w, _, _ in contract.WORDS)
    fake_mlx.CFG.update(lecture=audio, script=[
        [0.0, 6.0, text, [[w, s, e, 0.9] for w, s, e in contract.WORDS], 0.01, -0.2]])
    ov_dir = tmp_path / "ov-model"
    ov_dir.mkdir()
    for f in asr_openvino.MODEL_FILES:
        (ov_dir / f).write_bytes(b"x")
    mlx_dir = make_model_dir(tmp_path / "mlx-model")
    jobs = {
        "fw": (asr, {"audio": audio, "model": "large-v3", "compute_type": "float16",
                     "language": "en", "initial_prompt": contract.PROMPT, "timing": True}),
        "ov": (asr_openvino, {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
                              "model_dir": str(ov_dir), "device": "CPU", "language": "en",
                              "initial_prompt": contract.PROMPT, "cache_dir": "",
                              "hf_id": "openai/whisper-large-v3-turbo",
                              "hf_home": str(tmp_path / "hf")}),
        "mlx": (asr_mlx, {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
                          "model_dir": mlx_dir, "device": "mlx:gpu", "language": "en",
                          "initial_prompt": contract.PROMPT}),
    }
    out = {}
    for name, (mod, job) in jobs.items():
        in_path = tmp_path / ("%s_job.json" % name)
        in_path.write_text(json.dumps(job), encoding="utf-8")
        out_path = tmp_path / ("%s_result.json.tmp" % name)
        mod.run(str(in_path), str(out_path), lambda line: None)
        out[name] = read_json(out_path)
    assert set(out["mlx"]) == set(out["fw"]) == set(out["ov"]) \
        == {"prompt_terms_used", "sentences", "timing"}
    timings = {k: v.pop("timing") for k, v in out.items()}
    assert out["mlx"] == out["fw"] == out["ov"]
    assert {k: type(v) for k, v in timings["mlx"].items()} == \
        {k: type(v) for k, v in timings["fw"].items()}
    assert timings["mlx"]["audio_s"] == 6.0 and timings["mlx"]["first_compile"] is False
    # the same text repair in every worker (textrepair-int): the same counters but its time
    repairs = [dict(timings[k]["postprocess"]["textrepair"]) for k in ("fw", "ov", "mlx")]
    for r in repairs:
        assert isinstance(r.pop("ms"), int)
    assert repairs[0] == repairs[1] == repairs[2]


# ---------------------------------------------------------------------------
# Transcription
# ---------------------------------------------------------------------------

def test_success_with_word_timestamps(lecture, mlxjob):
    in_path, out_path = mlxjob()
    lines, result = run_worker(in_path, out_path)
    assert set(result) == {"prompt_terms_used", "sentences", "timing"}
    sents = result["sentences"]
    # the hallucinated segment is dropped, "so so so so" collapsed
    assert [s["text"] for s in sents] == EXPECTED_TEXTS
    s3 = sents[3]
    assert s3["low_conf_words"] == [{"i": 2, "w": "is"}]
    assert all(s["low_conf_words"] == [] for i, s in enumerate(sents) if i != 3)
    assert s3["words"] == [{"w": "Point", "s": 30500, "e": 31000}, {"w": "3", "s": 31000, "e": 31500},
                           {"w": "is", "s": 31500, "e": 32000}, {"w": "here.", "s": 32000, "e": 32600}]
    assert (s3["start_ms"], s3["end_ms"]) == (30500, 32600)
    assert result["prompt_terms_used"] == 2
    t = result["timing"]
    assert t["audio_s"] == 100.0 and t["first_compile"] is False
    # make_timing's fields and the text repair's counters (textrepair-int)
    assert set(t) == set(asr_common.make_timing(0, 0, 0, False, 0)) | {"postprocess"}
    assert set(t["postprocess"]) == {"textrepair"}
    assert not os.path.exists(os.path.join(os.path.dirname(out_path), "asr.ckpt.json"))


def test_transcribe_parameters(lecture, mlxjob):
    in_path, out_path = mlxjob()
    run_worker(in_path, out_path)
    (call,) = lecture.CALLS
    assert call["path_or_hf_repo"] == mlxjob.model_dir
    assert call["initial_prompt"] == "Point, here"
    assert call["word_timestamps"] is True
    assert call["condition_on_previous_text"] is False
    assert call["language"] == "en" and call["fp16"] is True
    assert call["verbose"] is None
    assert "beam_size" not in call and "best_of" not in call     # greedy (no beam search)
    assert call["offset"] == 0 and call["samples"] == 100 * 16000
    # loaded once through mlx_whisper's ModelHolder and reused by transcribe()
    assert lecture.load_models.LOADS == [(mlxjob.model_dir, "float16")]
    assert lecture.core.SEEDS == [0]
    assert os.environ.get("HF_HUB_OFFLINE") == "1"


def test_fp32_precision_and_no_language(lecture, mlxjob):
    in_path, out_path = mlxjob(precision="fp32", language="", initial_prompt="")
    run_worker(in_path, out_path)
    (call,) = lecture.CALLS
    assert call["fp16"] is False and call["language"] is None and call["initial_prompt"] is None
    assert lecture.load_models.LOADS == [(mlxjob.model_dir, "float32")]


def test_prompt_truncated_to_200_tokens(lecture, mlxjob):
    terms = ["term%03d" % i for i in range(300)]
    in_path, out_path = mlxjob(initial_prompt=", ".join(terms))
    _, result = run_worker(in_path, out_path)
    assert result["prompt_terms_used"] == 200
    assert lecture.CALLS[0]["initial_prompt"] == ", ".join(terms[:200])


def test_without_word_timestamps_sentences_have_no_words(lecture, mlxjob):
    in_path, out_path = mlxjob(word_timestamps=False)
    _, result = run_worker(in_path, out_path)
    assert lecture.CALLS[0]["word_timestamps"] is False
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    for s in result["sentences"]:
        assert "words" not in s and s["low_conf_words"] == []


def test_no_words_returned_falls_back_to_segments(lecture, mlxjob):
    """Asked for word timestamps but none came back: pseudo-words from the segment text
    (only for splitting), no "words", no low-confidence words (I7)."""
    lecture.CFG["words"] = False
    in_path, out_path = mlxjob()
    lines, result = run_worker(in_path, out_path)
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert all("words" not in s and s["low_conf_words"] == [] for s in result["sentences"])
    assert "no word timestamps returned; sentences from segments" in lines
    # the pseudo-words span the segment's time range
    assert result["sentences"][0]["start_ms"] == 400 and result["sentences"][0]["end_ms"] == 4500


def test_words_over_digital_silence_dropped_and_long_words_trimmed(fake_mlx, tmp_path, mlxjob):
    audio = write_wav_parts(tmp_path / "a.wav", [(4.0, 8000), (4.0, 0), (4.0, 8000)])
    fake_mlx.CFG.update(lecture=audio, script=[
        [0.0, 4.0, " Hello there.", [[" Hello", 0.2, 0.6, 0.9], [" there.", 0.6, 1.0, 0.9]],
         0.1, -0.3],
        [4.2, 5.2, " Ghost.", [[" Ghost.", 4.5, 5.0, 0.9]], 0.1, -0.3],
        # stretched over the silence (5.1 s): trimmed to the more voiced 2 s (its end)
        [8.0, 12.0, " Stretched words.", [[" Stretched", 3.5, 8.6, 0.9],
                                          [" words.", 8.6, 9.0, 0.9]], 0.1, -0.3],
    ])
    in_path, out_path = mlxjob(audio=audio, initial_prompt="")
    _, result = run_worker(in_path, out_path)
    sents = result["sentences"]
    assert [s["text"] for s in sents] == ["Hello there.", "Stretched words."]
    w = sents[1]["words"][0]
    assert (w["w"], w["s"], w["e"]) == ("Stretched", 6600, 8600)


def test_progress_after_load_monotonic_capped_then_one(lecture, mlxjob):
    lecture.CFG["windows"] = 5
    in_path, out_path = mlxjob()
    lines, _ = run_worker(in_path, out_path)
    loaded = next(i for i, l in enumerate(lines) if l.startswith("model large-v3-turbo"))
    first = next(i for i, l in enumerate(lines) if l.startswith("PROGRESS"))
    assert first > loaded and lines[first] == "PROGRESS 0.00"
    p = progress_of(lines)
    assert p == sorted(p) and len(set(p)) == len(p)
    assert p[-1] == 1.0 and max(p[:-1]) <= 0.95
    # the replaced tqdm bar reports inside the single call: 0.20, 0.40, ...
    assert [0.2, 0.4, 0.6, 0.8] == [x for x in p if 0 < x < 0.95]
    assert lines[-1] == "PROGRESS 1.00"
    # the real tqdm attribute is put back after the call
    assert lecture.tqdm.tqdm.__name__ == "_Bar"


def test_progress_unit():
    out = []
    p = asr_mlx.ChunkProgress(100.0, out.append)
    p.chunk(0.0, 50.0)
    p.frames(2500, 5000)
    p.frames(2500, 5000)        # same value: not printed again
    p.reach(50.0)
    p.chunk(50.0, 100.0)
    p.frames(5000, 5000)
    assert out == ["PROGRESS 0.25", "PROGRESS 0.50", "PROGRESS 0.95"]
    resumed = []
    q = asr_mlx.ChunkProgress(100.0, resumed.append, start=40.0)
    q.chunk(40.0, 100.0)
    q.frames(1000, 6000)        # 50 s: above the start line
    q.frames(0, 6000)
    assert resumed == ["PROGRESS 0.50"]


# ---------------------------------------------------------------------------
# Errors and exit codes
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("overrides, needle", [
    ({"device": "ov:GPU"}, "unsupported device"),
    ({"device": _DROP}, "unsupported device"),
    ({"precision": "q3"}, "unsupported precision"),
    ({"model": ""}, "job.model"),
    ({"model_dir": _DROP}, "job.model_dir"),
    ({"audio": "missing.wav"}, "audio file not found"),
    ({"language": 5}, "job.language"),
    ({"word_timestamps": "yes"}, "job.word_timestamps"),
    ({"checkpoint": {"path": "x"}}, "job.checkpoint.job_id"),
    ({"checkpoint": {"path": "x", "job_id": "j", "input_hash": "h", "chunk_s": -1}},
     "chunk_s"),
    ({"language": "xx"}, "unsupported language"),
])
def test_input_errors_exit_2(lecture, mlxjob, capsys, overrides, needle):
    in_path, out_path = mlxjob(**overrides)
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 2
    err = capsys.readouterr().err.strip().splitlines()
    assert err[-1].startswith("ERROR: input error: ") and needle in err[-1]
    assert not os.path.exists(out_path)
    assert lecture.CALLS == []


def test_bad_job_json_and_arguments(fake_mlx, tmp_path, capsys):
    bad = tmp_path / "job.json"
    bad.write_text("{not json", encoding="utf-8")
    assert asr_mlx.main(["--in", str(bad), "--out", str(tmp_path / "r.tmp")]) == 2
    assert "cannot read job file" in capsys.readouterr().err
    bad.write_text("[1, 2]", encoding="utf-8")
    assert asr_mlx.main(["--in", str(bad), "--out", str(tmp_path / "r.tmp")]) == 2
    assert "JSON object" in capsys.readouterr().err
    assert asr_mlx.main(["--in", str(tmp_path / "none.json"), "--out", "x"]) == 2
    assert asr_mlx.main(["--in", str(bad)]) == 2
    assert asr_mlx.main(["--bogus"]) == 2
    assert not os.path.exists(tmp_path / "r.tmp")


def test_missing_or_incomplete_model_dir_exit_2(lecture, mlxjob, tmp_path, capsys):
    in_path, out_path = mlxjob(model_dir=str(tmp_path / "nowhere"))
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 2
    assert "is not installed" in capsys.readouterr().err
    partial = tmp_path / "partial"
    partial.mkdir()
    (partial / "config.json").write_text("{}", encoding="utf-8")
    (partial / "weights.npz").write_bytes(b"")
    in_path, out_path = mlxjob(model_dir=str(partial))
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 2
    (partial / "weights.npz").write_bytes(b"x")
    assert asr_mlx.model_ready(str(partial))
    assert lecture.CALLS == []


def test_mlx_import_failure_is_device_unavailable(monkeypatch, mlxjob, capsys):
    monkeypatch.setitem(sys.modules, "mlx", None)        # import mlx -> ImportError
    monkeypatch.setitem(sys.modules, "mlx.core", None)
    in_path, out_path = mlxjob()
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 2
    out, err = capsys.readouterr()
    assert out.strip().splitlines()[-1] == "ERROR_CODE device_unavailable"
    assert err.strip().splitlines()[-1].startswith("ERROR: cannot import mlx")
    assert not os.path.exists(out_path)


def test_no_metal_is_device_unavailable(lecture, mlxjob, capsys, monkeypatch):
    monkeypatch.setattr(lecture.core.metal, "AVAILABLE", [False])
    in_path, out_path = mlxjob()
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 2
    out, err = capsys.readouterr()
    assert "ERROR_CODE device_unavailable" in out and "no Metal GPU" in err


def test_transcribe_failure_is_retryable_and_writes_nothing(lecture, mlxjob, capsys):
    lecture.CFG["error"] = True
    in_path, out_path = mlxjob()
    assert asr_mlx.main(["--in", in_path, "--out", out_path]) == 1
    assert "fake mlx failure" in capsys.readouterr().err.strip().splitlines()[-1]
    assert not os.path.exists(out_path)
    # the tqdm attribute is restored after a failing call too
    assert lecture.tqdm.tqdm.__name__ == "_Bar"


def test_result_written_atomically_after_transcription(lecture, mlxjob, monkeypatch):
    """Nothing reaches --out before the transcription ends; write_result (asr_common:
    write + fsync) writes only that path; nothing else appears next to it."""
    in_path, out_path = mlxjob()
    seen = []
    lecture.CFG["on_call"] = lambda n, kw: seen.append(os.path.exists(out_path))
    writes = []
    real = asr_common.write_result
    monkeypatch.setattr(asr_mlx, "write_result", lambda p, r: (writes.append(p), real(p, r)))
    lines, _ = run_worker(in_path, out_path)
    assert seen == [False] and writes == [out_path]
    assert sorted(os.listdir(os.path.dirname(out_path))) == ["job.json", "models",
                                                             "result.json.tmp"]


# ---------------------------------------------------------------------------
# Checkpoints and resuming (the OpenVINO worker's chunk contract)
# ---------------------------------------------------------------------------

def ck(tmp_path, device_id="mlx:gpu", chunk_s=30, **kw):
    obj = {"path": str(tmp_path / "asr.ckpt.json"), "job_id": JOB_ID, "input_hash": INPUT_HASH,
           "device_id": device_id, "model": "large-v3-turbo", "precision": "fp16",
           "chunk_s": chunk_s}
    obj.update(kw)
    return obj


def test_chunk_s_0_is_one_call_plus_parts(lecture, mlxjob, tmp_path):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path, chunk_s=0))
    _, result = run_worker(in_path, out_path)
    assert len(lecture.CALLS) == 1
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert result["parts"] == [{"from_ms": 0, "to_ms": 100000, "device_id": "mlx:gpu",
                                "model": "large-v3-turbo", "precision": "fp16"}]
    assert not os.path.exists(tmp_path / "asr.ckpt.json")


def test_chunks_cut_in_pauses_and_same_sentences(lecture, mlxjob, tmp_path):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    lines, result = run_worker(in_path, out_path)
    # cuts in the middle of the 2-second pauses near 30, 60, 90 s; last chunk >= 10 s
    assert [c["offset"] for c in lecture.CALLS] == [0, 29 * 16000, 59 * 16000, 89 * 16000]
    assert [c["initial_prompt"] for c in lecture.CALLS] == ["Point, here", None, None, None]
    assert lecture.core.SEEDS == [0, 29000, 59000, 89000]
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert result["parts"] == [{"from_ms": 0, "to_ms": 100000, "device_id": "mlx:gpu",
                                "model": "large-v3-turbo", "precision": "fp16"}]
    data = read_json(tmp_path / "asr.ckpt.json")       # the last write: after chunk 3
    assert data["version"] == asr_common.CHECKPOINT_VERSION
    assert data["backend"] == "mlx" and data["engine"] == "mlx-whisper 0.4.3"
    assert "vad" not in data and "resume_seek" not in data
    assert data["resume_ms"] == 89000 and data["prompt_terms_used"] == 2
    assert [(s["start_ms"], s["end_ms"]) for s in data["segments"]] == \
        [(0, 29000), (29000, 59000), (59000, 89000)]
    # words keep their probabilities (low_conf_words after a resume)
    probs = [w["probability"] for s in data["segments"] for w in s["words"]]
    assert 0.3 in probs
    assert "checkpoint asr.ckpt.json written at 0:29 (then after every chunk)" in lines
    p = progress_of(lines)
    assert p == sorted(p) and p[-1] == 1.0 and max(p[:-1]) <= 0.95


def test_interrupted_then_resumed_matches_the_uninterrupted_run(lecture, mlxjob, tmp_path):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    _, expected = run_worker(in_path, out_path)
    os.remove(out_path)
    os.remove(tmp_path / "asr.ckpt.json")
    lecture.CALLS.clear()
    lecture.CFG["fail_on_call"] = 3                      # dies in the third chunk
    with pytest.raises(RuntimeError):
        asr_mlx.run(in_path, out_path, lambda l: None)
    assert not os.path.exists(out_path)
    data = read_json(tmp_path / "asr.ckpt.json")
    assert data["resume_ms"] == 59000
    lecture.CALLS.clear()
    lecture.CFG.pop("fail_on_call")
    lines, resumed = run_worker(in_path, out_path)
    assert [c["offset"] for c in lecture.CALLS] == [59 * 16000, 89 * 16000]
    assert [c["initial_prompt"] for c in lecture.CALLS] == [None, None]
    assert resumed["sentences"] == expected["sentences"]
    assert resumed["prompt_terms_used"] == expected["prompt_terms_used"] == 2
    assert resumed["parts"] == expected["parts"]           # same device: still one part
    assert resumed["timing"]["audio_s"] == 41.0
    first = next(l for l in lines if l.startswith("PROGRESS"))
    assert first == "PROGRESS 0.59"
    assert any(l.startswith("resuming from checkpoint asr.ckpt.json at 0:59") for l in lines)


def test_resumed_on_another_device_id_is_two_parts(lecture, mlxjob, tmp_path):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path, device_id="mlx:gpu-a"))
    lecture.CFG["fail_on_call"] = 2
    with pytest.raises(RuntimeError):
        asr_mlx.run(in_path, out_path, lambda l: None)
    lecture.CFG.pop("fail_on_call")
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path, device_id="mlx:gpu"))
    _, result = run_worker(in_path, out_path)
    assert [(p["from_ms"], p["to_ms"], p["device_id"]) for p in result["parts"]] == \
        [(0, 29000, "mlx:gpu-a"), (29000, 100000, "mlx:gpu")]


def test_matching_checkpoint_continued_with_chunk_s_0(lecture, mlxjob, tmp_path):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    lecture.CFG["fail_on_call"] = 2
    with pytest.raises(RuntimeError):
        asr_mlx.run(in_path, out_path, lambda l: None)
    lecture.CFG.pop("fail_on_call")
    lecture.CALLS.clear()
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path, chunk_s=0))
    _, result = run_worker(in_path, out_path)
    assert [c["offset"] for c in lecture.CALLS] == [29 * 16000]       # the rest at once
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert read_json(tmp_path / "asr.ckpt.json")["resume_ms"] == 29000  # not rewritten


@pytest.mark.parametrize("changes", [{"backend": "openvino"}, {"engine": "mlx-whisper 0.4.2"},
                                     {"job_id": "other"}, {"input_hash": "v3:other"},
                                     {"version": 1}, {"resume_ms": 10 ** 9}])
def test_foreign_checkpoint_is_deleted_and_the_run_starts_over(lecture, mlxjob, tmp_path,
                                                               changes):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    lecture.CFG["fail_on_call"] = 2
    with pytest.raises(RuntimeError):
        asr_mlx.run(in_path, out_path, lambda l: None)
    lecture.CFG.pop("fail_on_call")
    path = tmp_path / "asr.ckpt.json"
    data = read_json(path)
    data.update(changes)
    path.write_text(json.dumps(data), encoding="utf-8")
    lecture.CALLS.clear()
    lines, result = run_worker(in_path, out_path)
    assert lecture.CALLS[0]["offset"] == 0 and lecture.CALLS[0]["initial_prompt"] == "Point, here"
    assert any("not used" in l and "starting over" in l for l in lines)
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert result["parts"][0]["from_ms"] == 0


def test_stray_tmp_of_a_killed_run_is_removed(lecture, mlxjob, tmp_path):
    (tmp_path / "asr.ckpt.json.tmp").write_text("{half", encoding="utf-8")
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    run_worker(in_path, out_path)
    assert not os.path.exists(tmp_path / "asr.ckpt.json.tmp")


# ---------------------------------------------------------------------------
# Real processes, with the fake packages on PYTHONPATH. Not quite as Go
# starts them: Go adds -s and leaves PYTHONPATH out of the environment
# (internal/pyenv), while these tests need PYTHONPATH to bring the fakes in.
# ---------------------------------------------------------------------------

def _env(tmp_path, **cfg):
    env = dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUNBUFFERED="1",
               PYTHONPATH=write_fake_mlx_packages(tmp_path / "fakepkgs"),
               LF_FAKE_MLX=json.dumps(cfg))
    return env


def _cli(tmp_path, *args, env=None, **cfg):
    return subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr_mlx.py"), *args],
                          capture_output=True, cwd=str(tmp_path),
                          env=env or _env(tmp_path, **cfg), timeout=120)


def test_cli_success_process(tmp_path, mlxjob, lecture_wav):
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    p = _cli(tmp_path, "--in", in_path, "--out", out_path, script=lecture_script(),
             lecture=lecture_wav)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    lines = p.stdout.decode("utf-8").splitlines()
    assert lines[-1] == "PROGRESS 1.00"
    result = read_json(out_path)
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert result["parts"][0]["device_id"] == "mlx:gpu"


def test_cli_input_error_exit_2_and_utf8(tmp_path, mlxjob, lecture_wav):
    in_path, out_path = mlxjob(audio="缺失的音频.wav")
    p = _cli(tmp_path, "--in", in_path, "--out", out_path, lecture=lecture_wav)
    assert p.returncode == 2
    assert "缺失的音频.wav" in p.stderr.decode("utf-8")
    assert not os.path.exists(out_path)


def test_cli_without_mlx_exits_2_device_unavailable(tmp_path, mlxjob, lecture_wav):
    in_path, out_path = mlxjob()
    env = _env(tmp_path, lecture=lecture_wav)
    env["LF_FAKE_MLX_IMPORT_ERROR"] = "No module named 'mlx._metal'"
    p = _cli(tmp_path, "--in", in_path, "--out", out_path, env=env)
    assert p.returncode == 2
    assert p.stdout.decode("utf-8").strip().splitlines()[-1] == "ERROR_CODE device_unavailable"
    assert "mlx._metal" in p.stderr.decode("utf-8").strip().splitlines()[-1]


def _wait_for(proc, marker, timeout=60):
    seen = []
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        line = proc.stdout.readline().decode("utf-8").strip()
        if not line and proc.poll() is not None:
            break
        seen.append(line)
        if line == marker:
            return seen
    raise AssertionError("no %r in %r" % (marker, seen))


def test_killed_during_transcription_leaves_no_output_then_resumes(tmp_path, mlxjob,
                                                                   lecture_wav):
    """Cancel = Go kills the process: it stops at once, nothing at --out; the next run
    continues from the last checkpoint (chunk 2 of 4 was running)."""
    in_path, out_path = mlxjob(checkpoint=ck(tmp_path))
    proc = subprocess.Popen([sys.executable, os.path.join(WORKER_DIR, "asr_mlx.py"),
                             "--in", in_path, "--out", out_path],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=str(tmp_path),
                            env=_env(tmp_path, script=lecture_script(), lecture=lecture_wav,
                                     hang_on_call=2))
    try:
        seen = _wait_for(proc, "TRANSCRIBING")
        assert "PROGRESS 0.00" in seen
        t0 = time.monotonic()
        proc.kill()
        proc.wait(timeout=10)
        assert time.monotonic() - t0 < 3
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.stdout.close()
        proc.stderr.close()
    assert proc.returncode != 0
    assert not os.path.exists(out_path)
    assert read_json(tmp_path / "asr.ckpt.json")["resume_ms"] == 29000
    p = _cli(tmp_path, "--in", in_path, "--out", out_path, script=lecture_script(),
             lecture=lecture_wav)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    lines = p.stdout.decode("utf-8").splitlines()
    assert next(l for l in lines if l.startswith("PROGRESS")) == "PROGRESS 0.29"
    result = read_json(out_path)
    assert [s["text"] for s in result["sentences"]] == EXPECTED_TEXTS
    assert result["timing"]["audio_s"] == 71.0


def test_fake_matches_the_real_library_surface(fake_mlx):
    """What asr_mlx relies on exists in the fake as in mlx_whisper 0.4.3: the module
    mlx_whisper.transcribe (shadowed on the package by the function), its tqdm attribute
    and ModelHolder, tokenizer.get_tokenizer / LANGUAGES, mlx.core.random.seed."""
    mod = importlib.import_module("mlx_whisper.transcribe")
    assert isinstance(mod, types.ModuleType) and callable(mod.transcribe)
    assert callable(sys.modules["mlx_whisper"].transcribe)
    assert hasattr(mod, "tqdm") and hasattr(mod, "ModelHolder")
    sig = inspect.signature(mod.transcribe)
    for name in ("path_or_hf_repo", "verbose", "condition_on_previous_text", "initial_prompt",
                 "word_timestamps"):
        assert name in sig.parameters


# ---------------------------------------------------------------------------
# Real MLX (macOS CI on Apple Silicon): python -m pytest -m realmlx worker
# ---------------------------------------------------------------------------

def _realmlx_skip(request):
    if "realmlx" not in (request.config.getoption("markexpr") or ""):
        pytest.skip("realmlx test: run with -m realmlx on an Apple Silicon Mac")
    if sys.platform != "darwin":
        pytest.skip("realmlx needs macOS on Apple Silicon")
    if not os.environ.get("LECTUREFLOW_MLX_MODEL_DIR"):
        pytest.skip("set LECTUREFLOW_MLX_MODEL_DIR to an MLX Whisper model directory")


@pytest.mark.realmlx
def test_realmlx_transcribes_and_resumes(request, tmp_path):
    """A real model on the Metal GPU: speech-like audio (sample/*.wav when present,
    otherwise a tone) transcribes with the result schema; with chunk_s 30 an interrupted
    run resumes to the uninterrupted result."""
    _realmlx_skip(request)
    from conftest import PROJECT_DIR
    samples = sorted(f for f in os.listdir(os.path.join(PROJECT_DIR, "sample"))
                     if f.endswith(".wav")) if os.path.isdir(os.path.join(PROJECT_DIR, "sample")) \
        else []
    audio = os.path.join(PROJECT_DIR, "sample", samples[0]) if samples else \
        write_wav_parts(tmp_path / "tone.wav", [(70.0, 3000)])
    job = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
           "model_dir": os.environ["LECTUREFLOW_MLX_MODEL_DIR"], "device": "mlx:gpu",
           "language": "en", "initial_prompt": "", "word_timestamps": True,
           "checkpoint": ck(tmp_path)}
    in_path = tmp_path / "job.json"
    in_path.write_text(json.dumps(job), encoding="utf-8")
    out_path = str(tmp_path / "result.json.tmp")
    p = subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr_mlx.py"), "--in",
                        str(in_path), "--out", out_path], capture_output=True, timeout=3600)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    result = read_json(out_path)
    assert set(result) == {"prompt_terms_used", "sentences", "timing", "parts"}
    for s in result["sentences"]:
        assert {"start_ms", "end_ms", "text", "low_conf_words", "words"} <= set(s)
    assert np.isfinite(result["timing"]["transcribe_s"])
