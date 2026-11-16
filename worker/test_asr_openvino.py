# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
"""worker/asr_openvino.py with fake openvino / openvino_genai modules (brief 3b §4, §7 A).

No real OpenVINO, model or device is used here (SPEC §14); see test_realgpu_openvino.py
for the opt-in real-device tests.
"""
import json
import os
import re
import subprocess
import sys
import time

import numpy as np
import pytest

import asr_common
import asr_openvino as ov_worker
from asr_common import Word
from conftest import PROJECT_DIR, WORKER_DIR, write_fake_ov_packages, write_wav_parts

PROGRESS_RE = re.compile(r"^PROGRESS (0\.\d\d|1\.00)$")
LOUD = 8000          # int16 peak of the test tone (about -15 dBFS RMS)

# Words of a short lecture (all <= 2 s, so no long-word trimming applies).
WORDS = [(" Okay,", 0.0, 0.5), (" let's", 0.5, 0.9), (" get", 0.9, 1.2),
         (" started.", 1.2, 2.0), (" Today", 3.8, 4.2), (" we", 4.2, 4.4),
         (" talk", 4.4, 4.8), (" about", 4.8, 5.1), (" the", 5.1, 5.3), (" cash", 5.3, 5.8),
         (" hierarchy.", 5.8, 7.0)]
CHUNKS = [(0.0, 3.8, " Okay, let's get started."),
          (3.8, 10.0, " Today we talk about the cash hierarchy.")]
SENTENCES = [
    {"start_ms": 0, "end_ms": 2000, "text": "Okay, let's get started.", "low_conf_words": [],
     "words": [{"w": "Okay,", "s": 0, "e": 500}, {"w": "let's", "s": 500, "e": 900},
               {"w": "get", "s": 900, "e": 1200}, {"w": "started.", "s": 1200, "e": 2000}]},
    {"start_ms": 3800, "end_ms": 7000, "text": "Today we talk about the cash hierarchy.",
     "low_conf_words": [],
     "words": [{"w": "Today", "s": 3800, "e": 4200}, {"w": "we", "s": 4200, "e": 4400},
               {"w": "talk", "s": 4400, "e": 4800}, {"w": "about", "s": 4800, "e": 5100},
               {"w": "the", "s": 5100, "e": 5300}, {"w": "cash", "s": 5300, "e": 5800},
               {"w": "hierarchy.", "s": 5800, "e": 7000}]},
]


def make_model_dir(path, skip=()):
    os.makedirs(path, exist_ok=True)
    for f in ov_worker.MODEL_FILES:
        if f not in skip:
            with open(os.path.join(path, f), "wb") as fh:
                fh.write(b"x")
    return str(path)


@pytest.fixture
def ovjob(tmp_path):
    """Writes job.json (with overrides) for a ready model; returns (in_path, out_path)."""
    audio = write_wav_parts(tmp_path / "audio.wav", [(10.0, LOUD)])
    models = tmp_path / "models"
    model_dir = make_model_dir(models / "whisper-large-v3-turbo-fp16")

    def make(**overrides):
        data = {"audio": audio, "model": "large-v3-turbo", "precision": "fp16",
                "model_dir": model_dir, "device": "GPU", "language": "en",
                "initial_prompt": "cache, write-back, TLB",
                "cache_dir": str(models / "cache" / "GPU-large-v3-turbo-fp16"),
                "hf_id": "openai/whisper-large-v3-turbo", "hf_home": str(models / "hf_cache"),
                "word_timestamps": True, "min_free_bytes": 1}
        data.update(overrides)
        data = {k: v for k, v in data.items() if v is not _DROP}
        in_path = tmp_path / "job.json"
        in_path.write_text(json.dumps(data), encoding="utf-8")
        return str(in_path), str(tmp_path / "result.json.tmp")

    make.model_dir = model_dir
    make.models = str(models)
    return make


_DROP = object()


@pytest.fixture
def lecture(fake_ov):
    fake_ov.words = list(WORDS)
    fake_ov.chunks = list(CHUNKS)
    fake_ov.texts = ["".join(c[2] for c in CHUNKS)]
    return fake_ov


def run_main(capsys, *argv):
    rc = ov_worker.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out.splitlines(), err


def read_json(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


# --- success path ----------------------------------------------------------------

def test_success_writes_result_with_timing(lecture, ovjob, capsys, tmp_path):
    in_path, out_path = ovjob()
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    result = read_json(out_path)
    assert set(result) == {"prompt_terms_used", "sentences", "timing"}
    assert result["prompt_terms_used"] == 3
    assert result["sentences"] == SENTENCES
    timing = result["timing"]
    assert set(timing) == {"load_s", "transcribe_s", "audio_s", "first_compile",
                           "peak_working_set_mb", "postprocess"}
    # the text repair before the split (textrepair-int): no redecode in this worker
    assert set(timing["postprocess"]) == {"textrepair"}
    assert timing["audio_s"] == 10.0
    assert timing["first_compile"] is True            # empty cache_dir on the GPU
    assert isinstance(timing["peak_working_set_mb"], int)
    if sys.platform == "win32":
        assert timing["peak_working_set_mb"] > 0
    assert all(isinstance(timing[k], float) and timing[k] >= 0 for k in ("load_s", "transcribe_s"))
    progress = [l for l in lines if l.startswith("PROGRESS")]
    assert all(PROGRESS_RE.match(l) for l in progress)
    assert progress[-1] == "PROGRESS 1.00" and lines[-1] == "PROGRESS 1.00"
    assert not any(l.startswith("ERROR_CODE") for l in lines)
    # nothing but --out (and the compile cache dir) is written next to the inputs
    assert sorted(os.listdir(tmp_path)) == ["audio.wav", "job.json", "models", "result.json.tmp"]


def test_pipeline_and_generation_parameters(lecture, ovjob, capsys):
    in_path, out_path = ovjob()
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    (pipe,) = lecture.pipelines
    assert pipe.model_dir == ovjob.model_dir
    assert pipe.device == "GPU.0"                       # the Intel GPU, not "GPU"
    assert pipe.props == {"word_timestamps": True,
                          "CACHE_DIR": os.path.join(ovjob.models, "cache", "GPU-large-v3-turbo-fp16")}
    (call,) = lecture.generate_calls
    cfg = call["config"]
    assert cfg["language"] == "<|en|>" and cfg["task"] == "transcribe"
    assert cfg["return_timestamps"] is True and cfg["word_timestamps"] is True
    assert cfg["num_beams"] == 1                         # greedy (lead ruling 6)
    assert cfg["initial_prompt"] == "cache, write-back, TLB"
    assert cfg["hotwords"] is None                       # G4: initial_prompt only
    audio = call["audio"]
    assert audio.dtype == np.float32 and audio.ndim == 1 and len(audio) == 160000
    assert 0.2 < float(np.abs(audio).max()) <= 1.0


def test_cpu_has_no_cache_dir_and_never_compiles(lecture, ovjob, capsys):
    in_path, out_path = ovjob(device="CPU", cache_dir="")
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0, err
    assert lecture.pipelines[0].device == "CPU"
    assert lecture.pipelines[0].props == {"word_timestamps": True}
    assert "STATUS compiling_model" not in lines
    assert read_json(out_path)["timing"]["first_compile"] is False


def test_npu_device(lecture, ovjob, capsys):
    in_path, out_path = ovjob(device="NPU", cache_dir=os.path.join(ovjob.models, "cache", "NPU-x"))
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.pipelines[0].device == "NPU"


def test_intel_gpu_found_by_vendor_not_by_index(lecture, ovjob, capsys):
    from conftest import INTEL_GPU_PROPS, NVIDIA_GPU_PROPS, CPU_PROPS
    lecture.devices = {"CPU": dict(CPU_PROPS), "GPU.0": dict(NVIDIA_GPU_PROPS),
                       "GPU.1": dict(INTEL_GPU_PROPS)}
    in_path, out_path = ovjob()
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.pipelines[0].device == "GPU.1"


@pytest.mark.parametrize("device,devices", [
    ("GPU", ["CPU", "GPU.1", "NPU"]),     # only the NVIDIA card is a GPU
    ("NPU", ["CPU", "GPU.0", "GPU.1"]),
])
def test_missing_device_is_device_unavailable(lecture, ovjob, capsys, device, devices):
    all_devices = lecture.devices
    lecture.devices = {k: all_devices[k] for k in devices}
    in_path, out_path = ovjob(device=device)
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert [l for l in lines if l.startswith("ERROR_CODE")] == ["ERROR_CODE device_unavailable"]
    assert err.strip().splitlines()[-1].startswith("ERROR: OpenVINO device %s not found" % device)
    assert lecture.pipelines == [] and not os.path.exists(out_path)


def test_word_pieces_are_glued(fake_ov, ovjob, capsys):
    fake_ov.words = [(" A", 0.0, 0.3), (" two", 0.3, 0.6), ("-time", 0.6, 0.9),
                     (" pad.", 0.9, 1.4), (" X", 2.0, 2.2), ("-OR", 2.2, 2.5), (" works.", 2.5, 3.0)]
    fake_ov.chunks = [(0.0, 3.0, " A two-time pad. X-OR works.")]
    in_path, out_path = ovjob()
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    sents = read_json(out_path)["sentences"]
    assert [s["text"] for s in sents] == ["A two-time pad.", "X-OR works."]
    assert sents[0]["words"] == [{"w": "A", "s": 0, "e": 300}, {"w": "two-time", "s": 300, "e": 900},
                                 {"w": "pad.", "s": 900, "e": 1400}]
    assert sents[1]["words"][0] == {"w": "X-OR", "s": 2000, "e": 2500}


def test_repeats_removed_and_low_conf_always_empty(fake_ov, ovjob, capsys):
    fake_ov.words = [(" the", 0.0, 0.1), (" the", 0.1, 0.2), (" the", 0.2, 0.3), (" end.", 0.3, 1.0)]
    fake_ov.chunks = [(0.0, 1.0, " the the the end.")]
    in_path, out_path = ovjob()
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    (s,) = read_json(out_path)["sentences"]
    assert s["text"] == "the end." and s["low_conf_words"] == []


# --- progress and STATUS lines ----------------------------------------------------

def test_streamer_progress_monotonic_capped_and_after_load(fake_ov, tmp_path, ovjob, capsys):
    audio = write_wav_parts(tmp_path / "long.wav", [(100.0, LOUD)])
    fake_ov.windows = 6
    fake_ov.words = [(" Hello.", 1.0, 1.5)]
    fake_ov.chunks = [(0.0, 2.0, " Hello.")]
    seen_at_init = []
    fake_ov.on_init = lambda pipe: seen_at_init.append(capsys.readouterr().out)
    in_path, out_path = ovjob(audio=audio)
    rc = ov_worker.main(["--in", in_path, "--out", out_path])
    assert rc == 0
    before = seen_at_init[0].splitlines()
    after = capsys.readouterr().out.splitlines()
    assert not any(l.startswith("PROGRESS") for l in before)      # nothing before the load
    progress = [l for l in after if l.startswith("PROGRESS")]
    # 0.00 once loaded; then 6 windows x 30 s / 100 s, capped at 0.95; 1.00 at the end
    assert progress == ["PROGRESS 0.00", "PROGRESS 0.30", "PROGRESS 0.60", "PROGRESS 0.90",
                        "PROGRESS 0.95", "PROGRESS 1.00"]
    i_load = next(i for i, l in enumerate(after) if l.startswith("model large-v3-turbo"))
    assert after.index("PROGRESS 0.00") == i_load + 1


def test_window_progress_unit():
    lines = []
    p = ov_worker.WindowProgress(45.0, lines.append, "RUNNING")
    assert [p("x") for _ in range(4)] == ["RUNNING"] * 4
    assert lines == ["PROGRESS 0.67", "PROGRESS 0.95"]
    lines.clear()
    p = ov_worker.WindowProgress(0.0, lines.append, "RUNNING")
    p("x"), p("x")
    assert lines == ["PROGRESS 0.95"]


def test_status_compiling_model_before_load_when_cache_is_empty(lecture, ovjob, capsys):
    lines_at_init = []
    lecture.on_init = lambda pipe: lines_at_init.extend(capsys.readouterr().out.splitlines())
    in_path, out_path = ovjob()
    assert ov_worker.main(["--in", in_path, "--out", out_path]) == 0
    assert "STATUS compiling_model" in lines_at_init
    assert "STATUS preparing_model" not in lines_at_init       # the model was ready


def test_no_compiling_status_with_a_cached_blob(lecture, ovjob, capsys):
    cache = os.path.join(ovjob.models, "cache", "GPU-large-v3-turbo-fp16")
    os.makedirs(cache)
    with open(os.path.join(cache, "12345.blob"), "wb") as f:
        f.write(b"compiled")
    in_path, out_path = ovjob()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0
    assert "STATUS compiling_model" not in lines
    assert read_json(out_path)["timing"]["first_compile"] is False


def test_empty_blob_does_not_count_as_cached(tmp_path):
    (tmp_path / "a.blob").write_bytes(b"")
    (tmp_path / "b.cl_cache").write_bytes(b"x")
    assert not ov_worker.compile_cached(str(tmp_path))
    (tmp_path / "c.BLOB").write_bytes(b"x")
    assert ov_worker.compile_cached(str(tmp_path))
    assert not ov_worker.compile_cached("")
    assert not ov_worker.compile_cached(str(tmp_path / "missing"))


# --- prompt -------------------------------------------------------------------------

def test_prompt_truncated_to_200_tokens(lecture, ovjob, capsys):
    terms = ["term%d" % i for i in range(300)]
    in_path, out_path = ovjob(initial_prompt=", ".join(terms))
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.generate_calls[0]["config"]["initial_prompt"] == ", ".join(terms[:200])
    assert read_json(out_path)["prompt_terms_used"] == 200


def test_no_prompt(lecture, ovjob, capsys):
    in_path, out_path = ovjob(initial_prompt="")
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.generate_calls[0]["config"]["initial_prompt"] is None
    assert read_json(out_path)["prompt_terms_used"] == 0


def test_no_language_means_detection_and_unknown_language_is_input_error(lecture, ovjob, capsys):
    in_path, out_path = ovjob(language="")
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.generate_calls[0]["config"]["language"] is None
    os.remove(out_path)
    in_path, out_path = ovjob(language="xx")
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "unsupported language 'xx'" in err
    assert not os.path.exists(out_path)


# --- without word timestamps ----------------------------------------------------------

def test_without_word_timestamps_sentences_have_no_words(lecture, ovjob, capsys):
    in_path, out_path = ovjob(word_timestamps=False)
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert lecture.pipelines[0].props == {
        "CACHE_DIR": os.path.join(ovjob.models, "cache", "GPU-large-v3-turbo-fp16")}
    assert lecture.generate_calls[0]["config"]["word_timestamps"] is False
    sents = read_json(out_path)["sentences"]
    assert [s["text"] for s in sents] == ["Okay, let's get started.",
                                          "Today we talk about the cash hierarchy."]
    assert all("words" not in s for s in sents)          # Go interpolates (P4.4)
    assert sents[0]["start_ms"] == 0 and sents[0]["end_ms"] == 3800
    assert sents[1]["start_ms"] == 3800 and sents[1]["end_ms"] == 10000


def test_no_words_returned_falls_back_to_segments(fake_ov, ovjob, capsys):
    fake_ov.words = []
    fake_ov.chunks = list(CHUNKS)
    in_path, out_path = ovjob()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0
    sents = read_json(out_path)["sentences"]
    assert len(sents) == 2 and all("words" not in s for s in sents)
    assert "no word timestamps returned; sentences from segments" in lines


def test_no_speech_gives_empty_sentence_list(fake_ov, ovjob, capsys):
    in_path, out_path = ovjob()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0
    result = read_json(out_path)
    assert result["sentences"] == [] and result["prompt_terms_used"] == 3


# --- silence and long words (no VAD) -----------------------------------------------

def _audio(parts):
    out = []
    for seconds, amp in parts:
        n = int(round(seconds * 16000))
        if amp:
            t = np.arange(n) / 16000.0
            out.append((amp / 32768.0 * np.sin(2 * np.pi * 440.0 * t)).astype(np.float32))
        else:
            out.append(np.zeros(n, dtype=np.float32))
    return np.concatenate(out)


def _clamp(words, audio):
    frames = ov_worker.frame_rms(audio)
    return ov_worker.clamp_long_words(words, frames, ov_worker.voiced_threshold(frames))


def test_long_word_after_silence_is_trimmed_to_its_voiced_end():
    audio = _audio([(3.0, 0), (0.5, LOUD), (2.5, 0)])
    (w,) = _clamp([Word(" Hello", 0.0, 3.6, 1.0)], audio)
    assert 2.9 <= w.start <= 3.0 and 3.5 <= w.end <= 3.6


def test_long_word_before_silence_is_trimmed_to_its_voiced_start():
    audio = _audio([(1.0, 0), (0.5, LOUD), (8.0, 0)])
    (w,) = _clamp([Word(" so", 1.0, 9.0, 1.0)], audio)
    assert w.start == 1.0 and 1.5 <= w.end <= 1.6


def test_long_word_without_voiced_frames_keeps_its_end():
    audio = _audio([(1.0, LOUD), (6.0, 0)])          # loud elsewhere, silent under the word
    (w,) = _clamp([Word(" um", 1.5, 6.5, 1.0)], audio)
    assert (w.start, w.end) == (6.5 - ov_worker.MAX_WORD_SECONDS, 6.5)


def test_long_voiced_word_keeps_the_more_voiced_side():
    # voiced 1.0-4.0 and 5.0-5.3: the first 2 s of the trimmed word are more voiced
    audio = _audio([(1.0, 0), (3.0, LOUD), (1.0, 0), (0.3, LOUD), (1.7, 0)])
    (w,) = _clamp([Word(" noise", 1.0, 6.0, 1.0)], audio)
    assert (w.start, w.end) == (1.0, 1.0 + ov_worker.MAX_WORD_SECONDS)
    # equally voiced on both sides: the end is kept (stretched words start early, 3a)
    audio = _audio([(1.0, 0), (5.0, LOUD), (1.0, 0)])
    (w,) = _clamp([Word(" noise", 1.0, 6.0, 1.0)], audio)
    assert (w.start, w.end) == (6.0 - ov_worker.MAX_WORD_SECONDS, 6.0)


def test_normal_words_are_unchanged():
    audio = _audio([(3.0, LOUD)])
    words = [Word(" a", 0.0, 0.4, 1.0), Word(" b", 0.4, 2.4, 1.0)]
    assert _clamp(words, audio) == words


def test_silent_segment_is_dropped_only_below_the_absolute_floor(fake_ov, tmp_path, ovjob, capsys):
    fake_ov.words = [(" Hello", 0.0, 0.5), (" there.", 0.5, 1.0), (" Thank", 6.0, 6.5),
                     (" you.", 6.5, 7.0)]
    fake_ov.chunks = [(0.0, 5.0, " Hello there."), (5.0, 10.0, " Thank you.")]
    silent = write_wav_parts(tmp_path / "silent.wav", [(5.0, LOUD), (5.0, 0)])
    in_path, out_path = ovjob(audio=silent)
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert [s["text"] for s in read_json(out_path)["sentences"]] == ["Hello there."]
    # quiet room noise (about -50 dBFS) is above the floor: rather keep (§0.1)
    quiet = write_wav_parts(tmp_path / "quiet.wav", [(5.0, LOUD), (5.0, 100)])
    in_path, out_path = ovjob(audio=quiet)
    assert run_main(capsys, "--in", in_path, "--out", out_path)[0] == 0
    assert [s["text"] for s in read_json(out_path)["sentences"]] == ["Hello there.", "Thank you."]


def test_times_made_monotonic_and_inside_the_audio():
    words = [Word(" a", 1.0, 1.4, 1.0), Word(" b", 0.9, 1.2, 1.0), Word(" c", 1.3, 1.6, 1.0),
             Word(" d", 9.5, 11.0, 1.0)]
    out = ov_worker.make_monotonic(words, 10.0)
    assert [(w.start, w.end) for w in out] == [(1.0, 1.0), (1.0, 1.2), (1.3, 1.6), (9.5, 10.0)]
    for a, b in zip(out, out[1:]):
        assert a.start <= b.start and a.end <= b.start and a.start <= a.end


def test_glue_pieces_unit():
    out = ov_worker.glue_pieces([("Hello", 0.0, 0.2), (" ", 0.2, 0.2), (" wor", 0.3, 0.5),
                                 ("ld.", 0.5, 0.4)])
    assert out == [Word(" Hello", 0.0, 0.2, 1.0), Word(" world.", 0.3, 0.5, 1.0)]


def test_frame_rms_covers_every_sample():
    audio = _audio([(1.01, LOUD)])
    frames = ov_worker.frame_rms(audio)
    assert len(frames) == 51
    assert frames[:50].min() > 0.1
    assert len(ov_worker.frame_rms(np.zeros(0, dtype=np.float32))) == 0


# --- input errors: exit 2, nothing written ------------------------------------------

@pytest.mark.parametrize("overrides,needle", [
    ({"precision": "float16"}, "precision"),
    ({"precision": _DROP}, "precision"),
    ({"device": "gpu"}, "device"),
    ({"device": "GPU.0"}, "device"),
    ({"model": ""}, "model"),
    ({"model_dir": _DROP}, "model_dir"),
    ({"audio": "does/not/exist.wav"}, "audio file not found"),
    ({"language": 5}, "language"),
    ({"initial_prompt": ["a"]}, "initial_prompt"),
    ({"cache_dir": 3}, "cache_dir"),
    ({"word_timestamps": "yes"}, "word_timestamps"),
    ({"min_free_bytes": -1}, "min_free_bytes"),
    ({"min_free_bytes": True}, "min_free_bytes"),
])
def test_input_errors_exit_2(fake_ov, ovjob, capsys, overrides, needle):
    in_path, out_path = ovjob(**overrides)
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert err.strip().splitlines()[-1].startswith("ERROR: input error")
    assert needle in err
    assert fake_ov.pipelines == [] and not os.path.exists(out_path)
    assert not any(l.startswith(("ERROR_CODE", "STATUS")) for l in lines)


@pytest.mark.parametrize("content", [b"this is not audio at all" * 100, None])
def test_undecodable_or_wrong_rate_audio_exit_2(fake_ov, ovjob, tmp_path, capsys, content):
    bad = tmp_path / "bad.wav"
    if content is None:
        write_wav_parts(bad, [(1.0, LOUD)], rate=44100)
    else:
        bad.write_bytes(content)
    in_path, out_path = ovjob(audio=str(bad))
    rc, _, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2 and "cannot decode audio" in err
    assert fake_ov.pipelines == []


def test_read_wav_stereo_and_8bit(tmp_path):
    import wave
    p = tmp_path / "st.wav"
    with wave.open(str(p), "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(np.array([16384, -16384, 8192, 8192], dtype="<i2").tobytes())
    assert ov_worker.read_wav(str(p)).tolist() == [0.0, 0.25]
    p8 = tmp_path / "u8.wav"
    with wave.open(str(p8), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(1)
        w.setframerate(16000)
        w.writeframes(bytes([128, 192, 64]))
    assert ov_worker.read_wav(str(p8)).tolist() == [0.0, 0.5, -0.5]


def test_bad_job_json_and_missing_arguments(fake_ov, tmp_path, capsys):
    bad = tmp_path / "job.json"
    bad.write_text("{not json", encoding="utf-8")
    rc, _, err = run_main(capsys, "--in", str(bad), "--out", str(tmp_path / "r.tmp"))
    assert rc == 2 and "cannot read job file" in err
    assert run_main(capsys)[0] == 2
    assert run_main(capsys, "--in", str(bad))[0] == 2
    assert run_main(capsys, "--bogus")[0] == 2


# --- retryable errors -----------------------------------------------------------------

def test_generate_failure_is_retryable_and_writes_nothing(lecture, ovjob, capsys):
    lecture.windows = 2
    lecture.generate_error = RuntimeError("Exception from src\\core\\src\\runtime\\tensor.cpp")
    in_path, out_path = ovjob()
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 1
    assert err.strip().splitlines()[-1].startswith("ERROR: RuntimeError: Exception from")
    assert "PROGRESS 0.00" in lines and not any(l.startswith("ERROR_CODE") for l in lines)
    assert not os.path.exists(out_path)


def test_openvino_import_failure_exit_2(monkeypatch, ovjob, capsys):
    monkeypatch.setitem(sys.modules, "openvino", None)       # import raises ImportError
    in_path, out_path = ovjob()
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert err.strip().splitlines()[-1].startswith("ERROR: cannot import openvino")
    assert not any(l.startswith("ERROR_CODE") for l in lines)


# --- model preparation (lead rulings 4 and 11) ------------------------------------------

class FakeExport:
    """Stands in for optimum-intel's export: optionally "downloads" into the HF cache,
    then writes a converted model (or fails)."""

    def __init__(self, lines, download=True, error=None, files=None, before_return=None):
        self.lines, self.download, self.error = lines, download, error
        self.files = ov_worker.MODEL_FILES if files is None else files
        self.before_return = before_return
        self.calls = []

    def __call__(self, hf_id, precision, out_dir, cache_dir):
        self.calls.append({"hf_id": hf_id, "precision": precision, "out_dir": out_dir,
                           "cache_dir": cache_dir, "hf_home": os.environ.get("HF_HOME"),
                           "lines": list(self.lines)})
        if self.download:
            snap = os.path.join(cache_dir, "models--" + hf_id.replace("/", "--"), "snapshots", "abc")
            os.makedirs(snap, exist_ok=True)
            with open(os.path.join(snap, "model.safetensors"), "wb") as f:
                f.write(b"weights")
        if self.error:
            raise self.error
        make_model_dir(out_dir, skip=[f for f in ov_worker.MODEL_FILES if f not in self.files])
        if self.before_return:
            self.before_return()


HF_ENV_KEYS = ("HF_HOME", "HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE", "TRANSFORMERS_CACHE",
               "HF_HUB_DISABLE_TELEMETRY", "HF_HUB_DISABLE_SYMLINKS_WARNING",
               "HF_HUB_DISABLE_PROGRESS_BARS")


@pytest.fixture
def clean_hf_env(monkeypatch):
    """set_hf_env changes os.environ: restore every key it touches afterwards."""
    for k in HF_ENV_KEYS:
        monkeypatch.delenv(k, raising=False)


@pytest.fixture
def prep(lecture, ovjob, clean_hf_env, tmp_path):
    """A job whose model is not converted yet; run(emit=lines.append) collects stdout."""
    model_dir = os.path.join(ovjob.models, "whisper-small-int8")
    hf_home = os.path.join(ovjob.models, "hf_cache")
    in_path, out_path = ovjob(model="small", precision="int8", model_dir=model_dir,
                              hf_id="openai/whisper-small", hf_home=hf_home)
    lines = []
    from types import SimpleNamespace
    return SimpleNamespace(in_path=in_path, out_path=out_path, model_dir=model_dir,
                           hf_home=hf_home, lines=lines, state=lecture,
                           repo=os.path.join(hf_home, "hub", "models--openai--whisper-small"),
                           marker=ov_worker.download_marker(hf_home, "openai/whisper-small"))


def test_prepare_converts_into_temp_dir_then_renames(prep, monkeypatch):
    export = FakeExport(prep.lines)
    monkeypatch.setattr(ov_worker, "export_model", export)
    ov_worker.run(prep.in_path, prep.out_path, prep.lines.append)
    (call,) = export.calls
    assert call["hf_id"] == "openai/whisper-small" and call["precision"] == "int8"
    assert call["out_dir"] == "%s.tmp-%d" % (prep.model_dir, os.getpid())
    assert call["cache_dir"] == os.path.join(prep.hf_home, "hub")
    assert call["hf_home"] == prep.hf_home                       # HF_HOME set before export
    assert call["lines"][-2:] == ["STATUS preparing_model",
                                  "preparing small (int8) from openai/whisper-small"]
    assert ov_worker.model_ready(prep.model_dir)
    assert not os.path.exists(call["out_dir"])
    # this run downloaded the weights: deleted after the conversion
    assert not os.path.exists(prep.repo) and not os.path.exists(prep.marker)
    # order of the status lines: preparing, then compiling (empty GPU cache), then load
    i_prep = prep.lines.index("STATUS preparing_model")
    i_comp = prep.lines.index("STATUS compiling_model")
    i_prog = prep.lines.index("PROGRESS 0.00")
    assert i_prep < i_comp < i_prog
    timing = read_json(prep.out_path)["timing"]
    assert timing["peak_working_set_mb"] == 0                     # conversion run: unknown
    assert prep.state.pipelines[0].model_dir == prep.model_dir


def test_prepare_keeps_an_existing_download(prep, monkeypatch):
    os.makedirs(os.path.join(prep.repo, "snapshots", "old"))
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines))
    ov_worker.run(prep.in_path, prep.out_path, prep.lines.append)
    assert os.path.isdir(os.path.join(prep.repo, "snapshots", "old"))   # e.g. left by 3a
    assert not os.path.exists(prep.marker)


def test_ready_model_is_not_prepared(lecture, ovjob, monkeypatch, capsys):
    def export(*a):
        raise AssertionError("must not convert")
    monkeypatch.setattr(ov_worker, "export_model", export)
    in_path, out_path = ovjob()
    rc, lines, _ = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 0 and "STATUS preparing_model" not in lines
    assert read_json(out_path)["timing"]["peak_working_set_mb"] != 0 or sys.platform != "win32"


def test_incomplete_model_dir_is_prepared_and_replaced(prep, monkeypatch):
    make_model_dir(prep.model_dir, skip=["openvino_detokenizer.bin"])
    with open(os.path.join(prep.model_dir, "stale.txt"), "w") as f:
        f.write("old")
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines))
    ov_worker.run(prep.in_path, prep.out_path, prep.lines.append)
    assert ov_worker.model_ready(prep.model_dir)
    assert not os.path.exists(os.path.join(prep.model_dir, "stale.txt"))
    siblings = os.listdir(os.path.dirname(prep.model_dir))
    assert not [s for s in siblings if ".tmp-" in s or ".old-" in s]


def test_model_prepared_meanwhile_by_another_worker_is_reused(prep, monkeypatch):
    def other_worker_finishes():
        make_model_dir(prep.model_dir)
        with open(os.path.join(prep.model_dir, "other.txt"), "w") as f:
            f.write("other worker")
    monkeypatch.setattr(ov_worker, "export_model",
                        FakeExport(prep.lines, before_return=other_worker_finishes))
    ov_worker.run(prep.in_path, prep.out_path, prep.lines.append)
    assert os.path.isfile(os.path.join(prep.model_dir, "other.txt"))      # theirs was kept
    assert any("prepared meanwhile; reusing it" in l for l in prep.lines)
    assert not [s for s in os.listdir(os.path.dirname(prep.model_dir)) if ".tmp-" in s]
    assert os.path.isfile(prep.out_path)


def test_no_space_error_code_exit_2(prep, monkeypatch, capsys):
    export = FakeExport(prep.lines)
    monkeypatch.setattr(ov_worker, "export_model", export)
    monkeypatch.setattr(ov_worker, "disk_free", lambda path: 1234)
    in_path, out_path = prep.in_path, prep.out_path
    with open(in_path, encoding="utf-8") as f:
        data = json.load(f)
    data["min_free_bytes"] = 10 << 30
    with open(in_path, "w", encoding="utf-8") as f:
        json.dump(data, f)
    rc, lines, err = run_main(capsys, "--in", in_path, "--out", out_path)
    assert rc == 2
    assert "STATUS preparing_model" in lines
    codes = [l for l in lines if l.startswith("ERROR_CODE")]
    assert codes == ['ERROR_CODE no_space {"free_bytes": 1234, "need_bytes": 10737418240}']
    assert lines[-1] == codes[0]
    assert err.strip().splitlines()[-1].startswith("ERROR: not enough free space")
    assert export.calls == [] and not os.path.exists(out_path)


def test_disk_free_uses_the_nearest_existing_ancestor(tmp_path):
    assert ov_worker.disk_free(str(tmp_path / "a" / "b" / "c")) > 0


@pytest.mark.parametrize("error,exit_code", [
    (OSError("We couldn't connect to 'https://huggingface.co'"), 1),
    (RuntimeError("conversion failed"), 1),
    (ImportError("No module named 'torch'"), 2),
])
def test_prepare_failure_error_code(prep, monkeypatch, capsys, error, exit_code):
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines, error=error))
    rc, lines, err = run_main(capsys, "--in", prep.in_path, "--out", prep.out_path)
    assert rc == exit_code
    assert [l for l in lines if l.startswith("ERROR_CODE")] == ["ERROR_CODE model_prepare_failed"]
    assert str(error) in err.strip().splitlines()[-1]
    assert not os.path.exists(prep.model_dir) and not os.path.exists(prep.out_path)
    assert not [s for s in os.listdir(os.path.dirname(prep.model_dir)) if ".tmp-" in s]
    # the partial download stays for the retry, remembered as ours
    assert os.path.isdir(prep.repo) and os.path.isfile(prep.marker)


def test_download_of_a_failed_run_is_deleted_by_the_next_successful_one(prep, monkeypatch, capsys):
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines, error=OSError("offline")))
    assert run_main(capsys, "--in", prep.in_path, "--out", prep.out_path)[0] == 1
    assert os.path.isdir(prep.repo)
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines))
    assert run_main(capsys, "--in", prep.in_path, "--out", prep.out_path)[0] == 0
    assert not os.path.exists(prep.repo) and not os.path.exists(prep.marker)


def test_incomplete_conversion_is_retryable(prep, monkeypatch, capsys):
    files = [f for f in ov_worker.MODEL_FILES if "detokenizer" not in f]
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines, files=files))
    rc, lines, err = run_main(capsys, "--in", prep.in_path, "--out", prep.out_path)
    assert rc == 1 and "ERROR_CODE model_prepare_failed" in lines
    assert "openvino_detokenizer.xml" in err
    assert not os.path.exists(prep.model_dir)


def test_stale_temp_dirs_of_dead_workers_are_removed(prep, monkeypatch):
    dead = prep.model_dir + ".tmp-424242"
    alive = prep.model_dir + ".tmp-31337"
    for d in (dead, alive):
        os.makedirs(d)
    monkeypatch.setattr(ov_worker, "_pid_alive", lambda pid: pid == 31337)
    monkeypatch.setattr(ov_worker, "export_model", FakeExport(prep.lines))
    ov_worker.run(prep.in_path, prep.out_path, prep.lines.append)
    assert not os.path.exists(dead) and os.path.isdir(alive)


def test_pid_alive():
    assert ov_worker._pid_alive(os.getpid())
    p = subprocess.Popen([sys.executable, "-c", "pass"])
    p.wait()
    assert not ov_worker._pid_alive(p.pid)


def test_missing_hf_fields_when_preparing_is_input_error(prep, monkeypatch, capsys):
    with open(prep.in_path, encoding="utf-8") as f:
        data = json.load(f)
    data["hf_id"] = ""
    with open(prep.in_path, "w", encoding="utf-8") as f:
        json.dump(data, f)
    rc, _, err = run_main(capsys, "--in", prep.in_path, "--out", prep.out_path)
    assert rc == 2 and "hf_id" in err


def test_hf_env(clean_hf_env, monkeypatch, tmp_path):
    monkeypatch.setenv("HF_HUB_CACHE", "elsewhere")
    ov_worker.set_hf_env(str(tmp_path))
    assert os.environ["HF_HOME"] == str(tmp_path) and "HF_HUB_CACHE" not in os.environ
    assert ov_worker.hf_repo_dir("h", "openai/whisper-small") == os.path.join(
        "h", "hub", "models--openai--whisper-small")


# --- --probe -------------------------------------------------------------------------

def _probe(capsys, monkeypatch, can_convert=True):
    monkeypatch.setattr(ov_worker, "can_convert", lambda: can_convert)
    rc, lines, err = run_main(capsys, "--probe")
    assert rc == 0, err
    (line,) = [l for l in lines if l.startswith("PROBE ")]
    return json.loads(line[len("PROBE "):])


def test_probe_lists_intel_devices_and_ignores_nvidia(fake_ov, capsys, monkeypatch):
    out = _probe(capsys, monkeypatch)
    assert out == {
        "openvino": "2026.4.1-fake", "genai": "2026.4.1.0-fake", "can_convert": True,
        "devices": [
            {"name": "CPU", "kind": "cpu", "full_name": "Intel(R) Core(TM) Ultra 9 386H",
             "vendor": "0x8086", "luid": "", "driver_version": ""},
            {"name": "GPU.0", "kind": "igpu", "full_name": "Intel(R) Graphics (iGPU)",
             "vendor": "0x8086", "luid": "e220010000000000", "driver_version": ""},
            {"name": "NPU", "kind": "npu", "full_name": "Intel(R) AI Boost",
             "vendor": "0x8086", "luid": "d026010000000000", "driver_version": "1004841"},
        ],
        "ignored": [
            {"name": "GPU.1", "kind": "", "reason": "not_intel",
             "full_name": "NVIDIA GeForce RTX 5070 Ti Laptop GPU (dGPU)", "vendor": "0x10de",
             "luid": "f7d9270000000000", "driver_version": ""},
        ],
    }


def test_probe_second_intel_gpu_and_unreadable_device(fake_ov, capsys, monkeypatch):
    from conftest import INTEL_GPU_PROPS
    fake_ov.devices["GPU.2"] = dict(INTEL_GPU_PROPS, FULL_DEVICE_NAME="Intel(R) Arc(TM) A770",
                                    DEVICE_LUID="aa00000000000000", GPU_DRIVER_VERSION="32.0.101.1")
    fake_ov.devices["GPU.3"] = {"FULL_DEVICE_NAME": RuntimeError("device lost")}
    fake_ov.devices["GNA"] = {"FULL_DEVICE_NAME": "GNA"}            # other types: left out
    out = _probe(capsys, monkeypatch, can_convert=False)
    assert out["can_convert"] is False
    assert [d["name"] for d in out["devices"]] == ["CPU", "GPU.0", "NPU"]
    ignored = {d["name"]: d for d in out["ignored"]}
    assert set(ignored) == {"GPU.1", "GPU.2", "GPU.3"}
    assert ignored["GPU.2"]["reason"] == "second_intel_gpu"
    assert ignored["GPU.2"]["driver_version"] == "32.0.101.1"
    assert ignored["GPU.3"]["reason"] == "error" and "device lost" in ignored["GPU.3"]["error"]


def test_probe_vendor_from_full_name_without_architecture(fake_ov, capsys, monkeypatch):
    fake_ov.devices = {"GPU": {"FULL_DEVICE_NAME": "Intel(R) Iris(R) Xe Graphics"}}
    out = _probe(capsys, monkeypatch)
    assert out["devices"] == [{"name": "GPU", "kind": "igpu",
                               "full_name": "Intel(R) Iris(R) Xe Graphics", "vendor": "0x8086",
                               "luid": "", "driver_version": ""}]
    assert out["ignored"] == []


def test_can_convert_is_false_without_optimum(monkeypatch):
    monkeypatch.setitem(sys.modules, "optimum.intel", None)
    assert ov_worker.can_convert() is False


# --- real processes, with the fake packages on PYTHONPATH ---------------------------
# Not quite as Go starts them: Go adds -s and leaves PYTHONPATH out of the
# environment (internal/pyenv), while these tests need PYTHONPATH to bring the
# fakes in.

def _env(tmp_path, **cfg):
    env = dict(os.environ, PYTHONIOENCODING="utf-8", PYTHONUNBUFFERED="1",
               PYTHONPATH=write_fake_ov_packages(tmp_path / "fakepkgs"),
               LF_FAKE_OV=json.dumps(cfg))
    return env


def _cli(tmp_path, *args, **cfg):
    return subprocess.run([sys.executable, os.path.join(WORKER_DIR, "asr_openvino.py"), *args],
                          capture_output=True, cwd=str(tmp_path), env=_env(tmp_path, **cfg),
                          timeout=120)


def test_cli_probe_process(tmp_path):
    p = _cli(tmp_path, "--probe")
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    lines = p.stdout.decode("utf-8").splitlines()
    (line,) = [l for l in lines if l.startswith("PROBE ")]
    out = json.loads(line[6:])
    assert [d["name"] for d in out["devices"]] == ["CPU", "GPU.0", "NPU"]
    assert [d["reason"] for d in out["ignored"]] == ["not_intel"]
    assert isinstance(out["can_convert"], bool)


def test_cli_probe_without_openvino_exits_2(tmp_path):
    p = _cli(tmp_path, "--probe", import_error="DLL load failed while importing _pyopenvino")
    assert p.returncode == 2
    last = p.stderr.decode("utf-8").strip().splitlines()[-1]
    assert last.startswith("ERROR: cannot import openvino") and "DLL load failed" in last
    assert not any(l.startswith("PROBE") for l in p.stdout.decode("utf-8").splitlines())


def test_cli_success_process(tmp_path, ovjob):
    in_path, out_path = ovjob(device="CPU", cache_dir="")
    p = _cli(tmp_path, "--in", in_path, "--out", out_path)
    assert p.returncode == 0, p.stderr.decode("utf-8", "replace")
    lines = p.stdout.decode("utf-8").splitlines()
    assert lines[-1] == "PROGRESS 1.00"
    result = read_json(out_path)
    assert [s["text"] for s in result["sentences"]] == ["Hello."]
    assert result["timing"]["peak_working_set_mb"] > 0 or sys.platform != "win32"


def test_cli_input_error_exit_2_and_utf8(tmp_path, ovjob):
    in_path, out_path = ovjob(audio="缺失的音频.wav")
    p = _cli(tmp_path, "--in", in_path, "--out", out_path)
    assert p.returncode == 2
    assert "缺失的音频.wav" in p.stderr.decode("utf-8")
    assert not os.path.exists(out_path)


def test_killed_during_transcription_leaves_no_output(tmp_path, ovjob):
    """Cancel = Go kills the process (lead ruling 9): nothing at --out afterwards."""
    in_path, out_path = ovjob(device="CPU", cache_dir="")
    proc = subprocess.Popen([sys.executable, os.path.join(WORKER_DIR, "asr_openvino.py"),
                             "--in", in_path, "--out", out_path],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=str(tmp_path),
                            env=_env(tmp_path, hang=True))
    try:
        seen = []
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            line = proc.stdout.readline().decode("utf-8").strip()
            if not line:
                break
            seen.append(line)
            if line == "GENERATING":
                break
        assert "GENERATING" in seen and "PROGRESS 0.00" in seen, seen
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
    assert not [f for f in os.listdir(tmp_path) if f.startswith("result")]
