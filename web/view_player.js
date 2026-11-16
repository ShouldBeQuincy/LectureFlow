// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Bilingual player "#/play/{lecture_id}?t={ms}" (SPEC-v2 P0–P10).
//
// Layout (P2): video, subtitle band and controls in the main column, the
// transcript stream on the right (clamp(300px, 22vw, 420px)); theater mode
// (T) hides the transcript, windows narrower than 1000 px do so
// automatically and open it as an overlay (player.ctl.transcript); full screen (F) is
// requested for the whole player container, never for the <video>, so the
// self-drawn subtitles stay. Subtitles are drawn from GET /cues (P3, P4),
// driven by requestVideoFrameCallback (timeupdate + requestAnimationFrame
// where it is missing, and in audio mode). The page never scrolls.
//
// Audio mode (P8.1): when stream.mp4 is not ready (or /media fails) the
// sound comes from /media?audio=1 (audio.wav; the /audio clips when that is
// missing) and the video area shows the lecture's title and chapters; once
// the stream becomes ready the user can switch.
//
// "Transcribe only" (3.1.0, lecture_english.js): while the lecture has no
// Chinese and waits for a model provider, the subtitles and the transcript
// show the English only (the saved display setting is kept for the other
// lectures; C and the CC panel then switch between English and off).
//
// Second camera angle (ext web3, player_angles.js): a lecture with
// "angles" shows the other angle as picture in picture in video mode; a
// click (or V) swaps the two, the sound and the clock stay with the main
// video. Chapters (ext web3): headings in the transcript stream, segments
// on the timeline, and the current chapter's name in the top bar opens the
// chapter list.
//
// The resume position (P6.4) is saved every 5 s while playing, on pause and
// when leaving the page. Nothing here changes server state except the
// sentence actions of the context menu, chapter generation and the resume
// position; every list comes from the API.

import { api } from './api.js';
import { h, clear, toast, openDialog } from './dom.js';
import { t } from './i18n.js';
import { fmtDecimal } from './util.js';
import { openMenu, closeMenu } from './menu.js';
import { pIcon } from './player_icons.js';
import { videoSource, fileSource, audioSource } from './player_media.js';
import * as Subs from './player_subs.js';
import { createTimeline } from './player_timeline.js';
import { createTranscript, isModified, isPending } from './player_transcript.js';
import { createChapters, chapterList } from './player_chapters.js';
import { bindKeys, openHelp } from './player_keys.js';
import { fmtPlayerTime, sentenceStart, readPref, writePref, codeText } from './player_util.js';
import { citeRef, coursePart, copyKeepingFocus } from './view_transcript.js';
import { zh } from './zh_display.js';
import { lectureTagText } from './kinds.js';
import { englishOnly } from './lecture_english.js';
import { createAngles, lectureAngles } from './player_angles.js';
import { angleURL } from './api_ext.js';
import { canRetranslateCurrent, retranslateCurrent, heldToast } from './retranslate_current.js';
import { retranslateWatch, translationHeld } from './retranslate_watch.js';

const enc = encodeURIComponent;

const RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2]; // P6.2
const NARROW_BELOW = 1000; // P2.2
const SAVE_EVERY_MS = 5000; // P6.4
const RESUME_MIN_MS = 3000; // a saved position this close to the start is not offered
const RESUME_END_MARGIN_MS = 15000; // …nor one this close to the end (the lecture was finished)
const PREV_RESTART_MS = 1500; // P6.3: "A" first goes back to the start of the current sentence
const AUTO_OVERLAY_RATIO = 0.72; // P2.3: overlay when the band would shrink the picture below this share…
const AUTO_OVERLAY_MIN_PX = 240; // …or below this height (a very low window)

const PREF_THEATER = 'lectureflow.player.theater';
const PREF_RATE = 'lectureflow.player.rate';
const PREF_VOLUME = 'lectureflow.player.volume';

// The ?t= of the route, or null when the URL has none. (app.js parseRoute
// turns a missing t into 0: Number(null) === 0, so check the hash itself.)
function routeTime(r) {
  const q = (location.hash.split('?')[1] || '').split('#')[0];
  return new URLSearchParams(q).has('t') && r.t != null ? r.t : null;
}

export function mountPlayer(main, route, ctx) {
  const id = route.id;
  const startT = routeTime(route);
  let alive = true;
  let media = null;
  let rvfc = 0;
  let raf = 0;
  let useRaf = false;
  // A presented frame can be up to one frame before the time it was sought
  // to: paused after "previous sentence" at a sentence start, its mediaTime
  // would show the previous sentence. Frame times below the last seek
  // target read as the target until playback passes it (seekFloor).
  let seekFloor = null;
  let offMedia = [];
  let textSeq = 0;
  const retranslated = retranslateWatch(); // "translate again" asked: the translation it had (T25 S3)
  let lastSaved = -1;
  let lastClockSec = -1;
  let lastClockDur = -1;
  let waitTimer = 0;
  let noticeTimer = 0;
  let osdTimer = 0;
  const timers = new Map();

  const savedVol = readPref(PREF_VOLUME, null);
  const S = {
    lec: null,
    course: null,
    sentences: [],
    cues: [],
    t: startT || 0,
    scrub: null,
    playing: false,
    ready: false, // the start position has been applied (saving may begin)
    rate: RATES.includes(readPref(PREF_RATE, 1)) ? readPref(PREF_RATE, 1) : 1,
    volume: savedVol && Number.isFinite(savedVol.v) ? Math.max(0, Math.min(1, savedVol.v)) : 1,
    muted: !!(savedVol && savedVol.m),
    loop: null, // {idx, start, end}
    subs: Subs.loadSettings(),
    lastOn: null, // the last subtitle display that was not off (English-only lectures turn back on with it)
    theater: !!readPref(PREF_THEATER, false),
    narrow: window.innerWidth < NARROW_BELOW,
    fs: false,
    fsSide: false,
    sideOverlay: false,
    overlay: false,
    kind: null, // 'video' | 'audio'
    audioWhy: '', // why audio mode: '' (no stream) | 'video_failed'
    audioClips: false, // audio mode: /media?audio=1 failed, play the /audio clips instead
    curCue: -1,
    curSent: -1,
    curChapter: -2,
  };

  // ---------------------------------------------------------------- DOM

  const tip = (name, key) => (key ? t('player.tip', { name, key }) : name);

  function ctlBtn(iconName, name, key, onclick, extra = {}) {
    return h(
      'button',
      {
        type: 'button',
        class: 'pl-ctl' + (extra.class ? ' ' + extra.class : ''),
        'aria-label': name,
        title: tip(name, key),
        'aria-keyshortcuts': extra.aria || null,
        'aria-pressed': extra.pressed == null ? null : String(!!extra.pressed),
        dataset: extra.dataset,
        onclick,
      },
      pIcon(iconName, { size: extra.size || 18 }),
    );
  }

  const courseEl = h('span', { class: 'pl-course' });
  const refEl = h('span', { class: 'pl-ref' });
  const nameEl = h('span', { class: 'pl-name' });
  // ext web3: the current chapter's name opens the chapter list
  const chapterNow = h('button', { type: 'button', class: 'pl-chapter-now', hidden: true, 'aria-haspopup': 'menu', title: t('player.chap.now_tip'), onclick: () => chapters.openList(chapterNow) });
  const backLink = h('a', { class: 'pl-back', href: `#/lecture/${enc(id)}`, 'aria-label': t('player.back'), title: t('player.back') }, pIcon('arrow-left', { size: 18 }));

  const chapters = createChapters({
    lectureId: id,
    onSeek: (ms) => seekTo(ms),
    display: () => shownDisplay(),
    duration,
    time: () => S.t,
    onChange: onChaptersChanged,
  });
  const helpBtn = h('button', { type: 'button', class: 'pl-ctl pl-ctl-sm', 'aria-label': t('player.help.title'), title: tip(t('player.help.title'), '?'), onclick: () => openHelp({ angles: angles.available() }) }, pIcon('keyboard', { size: 17 }));
  const top = h('header', { class: 'pl-top' }, backLink, h('div', { class: 'pl-title' }, courseEl, refEl, nameEl, chapterNow), h('div', { class: 'pl-top-tools' }, chapters.btn, helpBtn));

  const frame = h('div', { class: 'pl-frame' });
  const sub = Subs.createSubtitleBlock();
  const spinner = h('div', { class: 'pl-spinner', hidden: true, 'aria-hidden': 'true' }, pIcon('loader-circle', { size: 34, class: 'spin', stroke: 1.6 }));
  const notice = h('div', { class: 'pl-notice', role: 'status', hidden: true });
  const osdEl = h('div', { class: 'pl-osd', 'aria-live': 'polite' });
  const stageMsg = h('div', { class: 'pl-stage-msg', role: 'alert', hidden: true });
  const stage = h('div', { class: 'pl-stage' }, frame, sub.el, spinner, notice, osdEl, stageMsg);
  // ext web3: the second camera angle (picture in picture)
  const angles = createAngles({
    url: (name) => angleURL(id, name),
    osd: (text) => osd(text),
    toast: (text, kind) => toast(text, kind),
    onLayout: () => onAnglesChanged(),
    tip,
  });
  frame.after(angles.box);

  const playBtn = ctlBtn('play', t('player.ctl.play'), `${t('player.key.space')} / K`, () => togglePlay(), { class: 'pl-ctl-play', size: 20 });
  // T23 general 13: ← (5 s back) on screen too, for touch and the mouse
  const backBtn = ctlBtn('rotate-ccw', t('player.ctl.back5'), '←', () => seekBy(-5000), { size: 17, class: 'pl-ctl-back' });
  const prevBtn = ctlBtn('skip-back', t('player.ctl.prev'), 'A', () => prevSentence(), { size: 17 });
  const nextBtn = ctlBtn('skip-forward', t('player.ctl.next'), 'D', () => nextSentence(), { size: 17 });
  const loopBtn = ctlBtn('repeat-1', t('player.ctl.loop'), 'R', () => toggleLoop(), { pressed: false });
  const clockEl = h('span', { class: 'pl-clock', 'aria-hidden': 'true' });
  const rateBtn = h('button', { type: 'button', class: 'pl-ctl pl-rate', 'aria-haspopup': 'menu', 'aria-label': t('player.ctl.rate'), title: tip(t('player.ctl.rate'), '[ / ]'), onclick: () => rateMenu() });
  const volBtn = ctlBtn('volume-2', t('player.ctl.mute'), null, () => setVolume(S.muted && S.volume === 0 ? 1 : S.volume, !S.muted));
  const volRange = h('input', { type: 'range', class: 'pl-range pl-vol-range', min: '0', max: '1', step: '0.05', 'aria-label': t('player.ctl.volume'), title: t('player.ctl.volume') });
  volRange.addEventListener('input', () => setVolume(Number(volRange.value), false));
  const ccBtn = ctlBtn(
    'captions',
    t('player.ctl.subs'),
    null,
    () => {
      ccPanel.toggle();
      ccBtn.setAttribute('aria-expanded', String(ccPanel.isOpen));
    },
    { dataset: { ccToggle: '1' } },
  );
  ccBtn.title = t('player.ctl.subs_tip');
  ccBtn.setAttribute('aria-haspopup', 'dialog');
  const citeBtn = ctlBtn('quote', t('player.ctl.cite'), 'Y', () => copyCite());
  const sideBtn = ctlBtn('panel-right', t('player.ctl.transcript'), null, () => toggleSideOverlay(), { pressed: false, class: 'pl-ctl-side' });
  const theaterBtn = ctlBtn('rectangle-horizontal', t('player.ctl.theater'), 'T', () => toggleTheater(), { pressed: S.theater, class: 'pl-ctl-theater' });
  const fsBtn = ctlBtn('maximize', t('player.ctl.fullscreen'), 'F', () => toggleFullscreen());

  const ccPanel = Subs.createSubsPanel({
    getSettings: () => S.subs,
    englishOnly: () => enOnly(),
    onChange: (patch) => setSubs(enOnly() && patch.display ? { display: patch.display === 'off' ? 'off' : onDisplay() } : patch),
    isOverlay: () => S.overlay,
    onClose: (refocus) => {
      ccBtn.setAttribute('aria-expanded', 'false');
      if (refocus) ccBtn.focus({ preventScroll: true });
    },
  });

  const timeline = createTimeline({
    duration,
    onScrub: (ms) => {
      S.scrub = ms;
      // P6.1: the subtitle band follows the drag; the jump happens on release
      if (ms != null) {
        showCueAt(ms);
        updateClock(ms);
      } else tick();
    },
    onSeek: (ms) => seekTo(ms),
    preview: previewAt,
  });

  const bar = h(
    'div',
    { class: 'pl-bar' },
    h('div', { class: 'pl-bar-group' }, playBtn, backBtn, prevBtn, nextBtn, loopBtn, clockEl),
    h('div', { class: 'pl-bar-spacer' }),
    h('div', { class: 'pl-bar-group' }, rateBtn, h('div', { class: 'pl-vol' }, volBtn, volRange), ccBtn, citeBtn, sideBtn, angles.button, theaterBtn, fsBtn),
  );
  const controls = h('div', { class: 'pl-controls' }, timeline.el, bar, ccPanel.el);

  const transcript = createTranscript({
    onSeek: (s) => seekTo(s.start_ms), // P5: the play state stays as it was
    menuItems: sentenceMenu,
    duration,
  });
  const sideClose = h('button', { type: 'button', class: 'icon-btn pl-side-close', 'aria-label': t('common.close'), title: t('common.close'), onclick: () => toggleSideOverlay(false) }, pIcon('x', { size: 16 }));
  const side = h('aside', { class: 'pl-side', 'aria-label': t('player.side.title') }, h('div', { class: 'pl-side-head' }, h('h2', { class: 'pl-side-title' }, t('player.side.title')), sideClose), transcript.el);

  const mainCol = h('div', { class: 'pl-main' }, stage, controls);
  const body = h('div', { class: 'pl-body' }, mainCol, side);
  const root = h('div', { class: 'pl', tabindex: '-1' }, top, body);
  main.append(root);

  // ---------------------------------------------------------------- time

  function lastEnd() {
    const c = S.cues[S.cues.length - 1];
    const s = S.sentences[S.sentences.length - 1];
    return Math.max(c ? c.end_ms : 0, s ? s.end_ms : 0);
  }

  function totalMs() {
    return (S.lec && S.lec.duration_ms) || lastEnd();
  }

  function duration() {
    const d = media ? media.durationMs() : 0;
    return d || totalMs();
  }

  function tick(ms) {
    if (!alive) return;
    if (ms == null || !Number.isFinite(ms)) ms = media ? media.currentMs() : S.t;
    // P6.2 single-sentence loop
    if (S.loop && S.scrub == null && media && !media.paused && ms >= S.loop.end) {
      media.seek(S.loop.start);
      ms = seekFloor = S.loop.start;
    }
    S.t = ms;
    if (S.scrub == null) {
      showCueAt(ms);
      timeline.setTime(ms);
      updateClock(ms);
    }
    const si = Subs.lastStartingBefore(S.sentences, ms);
    if (si !== S.curSent) {
      S.curSent = si;
      transcript.setCurrent(si);
    }
    const ci = chapters.currentIndex(ms);
    if (ci !== S.curChapter) {
      S.curChapter = ci;
      renderChapterNow();
      if (S.kind === 'audio') renderAudioPanel();
    }
  }

  function showCueAt(ms, force = false) {
    const i = Subs.cueAt(S.cues, ms);
    if (i === S.curCue && !force) return;
    S.curCue = i;
    sub.show(cueText(S.cues[i]));
  }

  // "Transcribe only": English only, whatever the saved display (3.1.0).
  function enOnly() {
    return englishOnly(S.lec);
  }

  // The display shown now: the setting, or English for an English-only lecture.
  function shownDisplay() {
    const d = S.subs.display;
    return enOnly() && d !== 'off' ? 'en' : d;
  }

  // The display to turn the subtitles back on with: the current setting
  // unless it is off, else the last one that was on.
  function onDisplay() {
    return S.subs.display !== 'off' ? S.subs.display : S.lastOn || 'both';
  }

  // What the band shows for the display setting (P3). Where the translation
  // is not done yet, "Chinese only" shows the English (P1).
  function cueText(c) {
    if (!c) return null;
    switch (shownDisplay()) {
      case 'off':
        return null;
      case 'en':
        return { en: c.en, zh: '' };
      case 'zh':
        return c.zh ? { en: '', zh: zh(c.zh) } : { en: c.en, zh: '' };
      default:
        return { en: c.en, zh: zh(c.zh) }; // P14.6 display script
    }
  }

  // P6.1 hover preview: the time, its chapter and the English sentence of
  // that moment (the last one started; ~60 characters).
  function previewAt(ms) {
    const si = Subs.lastStartingBefore(S.sentences, ms);
    const ci = chapters.currentIndex(ms);
    const c = ci >= 0 ? chapters.chapters()[ci] : null;
    return { text: si >= 0 ? sentenceStart(S.sentences[si].text) : '', chapter: c ? chapters.title(c) : '' };
  }

  function updateClock(ms) {
    const d = duration();
    const sec = Math.floor(ms / 1000);
    if (sec === lastClockSec && d === lastClockDur) return;
    lastClockSec = sec;
    lastClockDur = d;
    clockEl.textContent = `${fmtPlayerTime(ms, d)} / ${fmtPlayerTime(d, d)}`;
  }

  function startFrameLoop() {
    stopFrameLoop();
    // a main video that is not on screen (the second angle in the picture,
    // no picture in picture) presents no frames: animation frames instead
    useRaf = !(media.kind === 'video' && typeof media.el.requestVideoFrameCallback === 'function' && !angles.mainHidden());
    if (!useRaf) {
      const el = media.el;
      const step = (now, meta) => {
        if (!alive || !media || media.el !== el) return;
        let ms = meta && Number.isFinite(meta.mediaTime) ? meta.mediaTime * 1000 : undefined;
        if (ms != null && seekFloor != null) {
          if (ms < seekFloor) ms = seekFloor;
          else seekFloor = null;
        }
        tick(ms);
        rvfc = el.requestVideoFrameCallback(step);
      };
      rvfc = el.requestVideoFrameCallback(step);
    }
  }

  function stopFrameLoop() {
    if (rvfc && media && media.el && media.el.cancelVideoFrameCallback) media.el.cancelVideoFrameCallback(rvfc);
    rvfc = 0;
    cancelAnimationFrame(raf);
    raf = 0;
  }

  function rafLoop() {
    raf = 0;
    if (!alive || !media || media.paused) return;
    tick();
    raf = requestAnimationFrame(rafLoop);
  }

  // ---------------------------------------------------------------- media

  function setupMedia(kind, startMs, autoplay) {
    teardownMedia();
    S.kind = kind;
    S.ready = false;
    root.dataset.kind = kind;
    clear(frame);
    if (kind === 'video') {
      media = videoSource(api.mediaURL(id));
      frame.append(media.el);
      angles.attach(media, frame);
    } else {
      // audio.wav with Range requests; the /audio clips when it is missing
      media = S.audioClips ? audioSource(id, totalMs) : fileSource(api.mediaURL(id, true), 'audio');
      renderAudioPanel();
    }
    const clips = !media.el;
    media.setRate(S.rate);
    media.setVolume(S.volume, S.muted);
    const on = (type, fn) => offMedia.push(media.on(type, fn));
    on('loadedmetadata', () => {
      layout();
      timeline.setDuration(duration());
      if (!clips && !S.ready) {
        media.seek(startMs);
        seekFloor = startMs;
        S.ready = true;
        tick(startMs);
      }
    });
    on('durationchange', () => {
      timeline.setDuration(duration());
      updateClock(S.t);
    });
    on('play', () => setPlaying(true));
    on('pause', () => {
      setPlaying(false);
      tick();
      saveResume(false);
    });
    on('ended', () => {
      setPlaying(false);
      saveResume(false);
    });
    on('waiting', () => setWaiting(true));
    on('playing', () => {
      setWaiting(false);
      if (useRaf && !raf) raf = requestAnimationFrame(rafLoop);
    });
    on('seeked', () => {
      setWaiting(false);
      if (clips && !S.ready) S.ready = true;
      tick();
      timeline.setBuffered(media.buffered());
    });
    // also with requestVideoFrameCallback: frames are not delivered while the
    // page is not rendered (background window), the loop must still hold
    on('timeupdate', () => tick());
    on('progress', () => timeline.setBuffered(media.buffered()));
    on('error', (e) => onMediaError(kind, clips, e || {}));
    startFrameLoop();
    if (clips) media.load(startMs);
    S.t = startMs;
    tick(startMs);
    timeline.setDuration(duration());
    setWaiting(true);
    if (autoplay) media.play();
    layout();
  }

  function teardownMedia() {
    stopFrameLoop();
    for (const off of offMedia) off();
    offMedia = [];
    angles.detach();
    if (media) media.destroy();
    media = null;
    setPlaying(false);
  }

  function onMediaError(kind, clips, e) {
    if (!alive) return;
    if (!clips) {
      // /media missing or unplayable: continue in audio mode at the same
      // place (audio.wav first, then the /audio clips)
      const at = S.t;
      const wasPlaying = S.playing;
      if (kind === 'video') S.audioWhy = 'video_failed';
      else S.audioClips = true;
      setupMedia('audio', at, wasPlaying);
      return;
    }
    setWaiting(false);
    showStageMsg(t('player.audio.load_failed', { error: e.message || '' }), () => {
      hideStageMsg();
      setupMedia('audio', S.t, false);
    });
  }

  function setWaiting(on) {
    clearTimeout(waitTimer);
    if (!on) {
      spinner.hidden = true;
      return;
    }
    waitTimer = setTimeout(() => (spinner.hidden = false), 300);
  }

  function setPlaying(on) {
    S.playing = on;
    clear(playBtn).append(pIcon(on ? 'pause' : 'play', { size: 20 }));
    const name = on ? t('player.ctl.pause') : t('player.ctl.play');
    playBtn.setAttribute('aria-label', name);
    playBtn.title = tip(name, `${t('player.key.space')} / K`);
    root.classList.toggle('is-playing', on);
    if (on && useRaf && !raf) raf = requestAnimationFrame(rafLoop);
  }

  function togglePlay() {
    if (!media) return;
    if (media.paused) media.play();
    else media.pause();
  }

  // Every user seek goes through here. Moving to another sentence ends the
  // single-sentence loop (P6.2).
  function seekTo(ms) {
    const d = duration();
    ms = Math.max(0, d ? Math.min(ms, d - 50) : ms);
    if (S.loop && Subs.lastStartingBefore(S.sentences, ms) !== S.loop.idx) setLoop(null);
    hideNotice();
    if (!media) return;
    media.seek(ms);
    seekFloor = ms;
    tick(ms);
  }

  function seekBy(delta) {
    seekTo((media ? media.currentMs() : S.t) + delta);
  }

  // ---------------------------------------------------------------- sentences

  function currentSentence() {
    return Subs.lastStartingBefore(S.sentences, S.t);
  }

  function prevSentence() {
    const i = currentSentence();
    if (i < 0) return seekTo(0);
    const s = S.sentences[i];
    if (S.t - s.start_ms > PREV_RESTART_MS || i === 0) seekTo(s.start_ms);
    else seekTo(S.sentences[i - 1].start_ms);
  }

  function nextSentence() {
    const i = currentSentence();
    const n = S.sentences[i + 1];
    if (n) seekTo(n.start_ms);
  }

  function setLoop(i) {
    const s = i == null ? null : S.sentences[i];
    S.loop = s ? { idx: i, start: s.start_ms, end: s.end_ms } : null;
    loopBtn.setAttribute('aria-pressed', String(!!S.loop));
    loopBtn.classList.toggle('is-on', !!S.loop);
    root.classList.toggle('is-looping', !!S.loop);
    osd(S.loop ? t('player.osd.loop_on') : t('player.osd.loop_off'));
  }

  function toggleLoop() {
    if (S.loop) return setLoop(null);
    const i = Math.max(0, currentSentence());
    if (!S.sentences[i]) return;
    setLoop(i);
    const s = S.sentences[i];
    if (S.t < s.start_ms || S.t >= s.end_ms) seekTo(s.start_ms);
  }

  function loopFrom(s) {
    const i = S.sentences.indexOf(s);
    if (i < 0) return;
    setLoop(i);
    seekTo(s.start_ms);
    if (media && media.paused) media.play();
  }

  // ---------------------------------------------------------------- citation (A11 / P6.2)

  function courseForCite() {
    if (S.course) return S.course;
    return (ctx.courses() || []).find((c) => S.lec && c.id === S.lec.course_id) || null;
  }

  async function copyCite(s) {
    if (!S.lec) return;
    if (!s) s = S.sentences[Math.max(0, currentSentence())];
    if (!s) return;
    const ref = `[${citeRef(courseForCite(), S.lec.cite_label || S.lec.label, s.start_ms)}]`;
    const ok = await copyKeepingFocus(ref); // T23 serious 3: the sentence keeps the focus
    toast(ok ? t('player.cite_copied', { ref }) : t('player.copy_failed'), ok ? 'success' : 'warn');
  }

  async function copyPlain(text, what) {
    const ok = await copyKeepingFocus(text);
    toast(ok ? t(what) : t('player.copy_failed'), ok ? 'success' : 'warn');
  }

  // ---------------------------------------------------------------- sentence menu (A12.2 + P5)

  function sentenceMenu(s) {
    if (!s || !S.lec) return null;
    const can = S.lec.reviewable;
    const ref = citeRef(courseForCite(), S.lec.cite_label || S.lec.label, s.start_ms);
    return [
      {
        label: t('player.menu.play_here'),
        icon: 'play',
        onSelect: () => {
          seekTo(s.start_ms);
          if (media && media.paused) media.play();
        },
      },
      { label: t('player.menu.loop_here'), icon: 'rotate-cw', hint: 'R', onSelect: () => loopFrom(s) },
      'sep',
      { label: t('player.menu.copy_cite'), icon: 'quote', hint: `[${ref}]`, onSelect: () => copyCite(s) },
      { label: t('player.menu.copy_en'), icon: 'copy', onSelect: () => copyPlain(s.text, 'player.copied_en') },
      s.zh ? { label: t('player.menu.copy_zh'), icon: 'copy', onSelect: () => copyPlain(zh(s.zh), 'player.copied_zh') } : null,
      'sep',
      can ? { label: t('player.menu.edit'), icon: 'pencil', onSelect: () => editSentence(s) } : null,
      can && s.human_text != null ? { label: t('player.menu.reset'), icon: 'undo-2', hint: t('player.menu.reset_hint'), onSelect: () => sentenceOp(s, 'reset') } : null,
      can && (s.zh_status === 'ok' || s.zh_status === 'qa_failed') ? { label: t('player.menu.retranslate'), icon: 'languages', onSelect: () => sentenceOp(s, 'retranslate') } : null,
      can && canRetranslateCurrent(s) ? { label: t('player.menu.retranslate_current'), icon: 'languages', hint: t('player.menu.retranslate_current_hint'), onSelect: () => retranslateCurrent(id, s.idx, S.lec).then((u) => u && applySentence(u)) } : null,
      'sep',
      s.patch && s.patch.class === 'decide' ? { label: t('player.menu.open_review'), icon: 'list-checks', onSelect: () => (location.hash = `#/lecture/${enc(id)}/review`) } : null,
    ];
  }

  function editSentence(s) {
    // R78b medium 2: the lecture's translation held (stale, failed,
    // cancelled): the edit waits for its next run, the texts say so
    const wait = translationHeld(S.lec);
    openDialog(t('player.edit.title', { n: s.idx + 1 }), (close, busy) => {
      // round 9 WV-02 (T28 suggestion 4): the two notes under the box
      // describe it (one such dialog at a time: fixed ids)
      const ta = h('textarea', { class: 'input pl-edit-ta', rows: '4', 'aria-label': t('player.edit.label'), 'aria-describedby': 'pl-edit-hint pl-edit-zh-note', autofocus: true });
      ta.value = s.text;
      const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary' }, t('player.edit.save'));
      const submit = async (e) => {
        if (e) e.preventDefault();
        const text = ta.value.trim();
        if (!text) {
          toast(t('player.edit.empty'), 'warn');
          return;
        }
        saveBtn.disabled = true;
        try {
          const updated = await busy(api.sentenceEdit(id, s.idx, text));
          retranslated.drop(s.idx); // the English changed: its translation is a new one
          if (updated) applySentence(updated);
          close(true);
          if (wait) heldToast(t('player.edit.saved_held'));
          else toast(t('player.edit.saved'), 'success');
        } catch {
          saveBtn.disabled = false; // shown by the API layer; keep the dialog open
        }
      };
      ta.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) submit(e);
      });
      return h(
        'form',
        { onsubmit: submit },
        h('p', { class: 'dlg-msg pl-edit-asr' }, h('span', { class: 'muted' }, t('player.edit.asr')), ' ', h('span', { lang: 'en' }, s.asr_text)),
        ta,
        h('p', { class: 'dlg-hint', id: 'pl-edit-hint' }, wait ? t('player.edit.hint_held') : t('player.edit.hint')),
        // T22 S2, T23 general 7: the translation is not edited itself
        h('p', { class: 'dlg-hint', id: 'pl-edit-zh-note' }, wait ? t('player.edit.zh_note_held') : t('player.edit.zh_note')),
        h('div', { class: 'dlg-actions' }, h('button', { type: 'button', class: 'btn', onclick: () => close(null) }, t('common.cancel')), saveBtn),
      );
    });
  }

  async function sentenceOp(s, op) {
    const wait = translationHeld(S.lec); // R78b medium 2, as the lecture was when it was sent
    try {
      const updated = op === 'reset' ? await api.sentenceReset(id, s.idx) : await api.sentenceRetranslate(id, s.idx);
      if (op === 'retranslate' && !wait) retranslated.start(s);
      else retranslated.drop(s.idx);
      if (updated) applySentence(updated);
      // WV-01: the held texts stay until closed
      if (wait) heldToast(t(op === 'reset' ? 'player.menu.reset_done_held' : 'player.menu.retranslate_held'));
      else toast(t(op === 'reset' ? 'player.menu.reset_done' : 'player.menu.retranslate_done'), 'success');
    } catch {
      // shown by the API layer
    }
  }

  function applySentence(updated) {
    const i = S.sentences.findIndex((x) => x.idx === updated.idx);
    if (i < 0) return;
    S.sentences[i] = updated;
    transcript.updateSentence(updated);
    updateMarks();
    scheduleText(600); // the cues follow with SSE "sentences"; also refetch in case it does not come
  }

  // ---------------------------------------------------------------- subtitles settings

  function setSubs(patch) {
    const before = S.subs.display;
    if (before !== 'off') S.lastOn = before;
    S.subs = { ...S.subs, ...patch };
    Subs.saveSettings(S.subs);
    root.dataset.order = S.subs.order;
    root.style.setProperty('--pl-ov-alpha', String(S.subs.opacity));
    transcript.setDisplay(shownDisplay(), S.subs.order);
    sub.reset();
    layout();
    showCueAt(S.scrub != null ? S.scrub : S.t, true);
    ccPanel.refresh();
    if (before !== S.subs.display) {
      chapters.rerender();
      renderChapterNow();
      syncTranscriptChapters();
      if (S.kind === 'audio') renderAudioPanel();
    }
  }

  function cycleDisplay() {
    if (enOnly()) {
      // English only: C switches the subtitles off and on again
      setSubs({ display: shownDisplay() === 'off' ? onDisplay() : 'off' });
    } else {
      const i = Subs.DISPLAYS.indexOf(S.subs.display);
      setSubs({ display: Subs.DISPLAYS[(i + 1) % Subs.DISPLAYS.length] });
    }
    osd(t('player.osd.display', { mode: t(`player.cc.display_${shownDisplay()}`) }));
  }

  // The lecture became English-only or stopped being so (a provider was
  // set up, its first translation arrived): redraw what depends on it.
  function syncEnglishOnly(was) {
    if (was === enOnly()) return;
    transcript.setDisplay(shownDisplay(), S.subs.order);
    sub.reset();
    layout();
    showCueAt(S.scrub != null ? S.scrub : S.t, true);
    ccPanel.refresh();
    chapters.rerender();
    renderChapterNow();
    syncTranscriptChapters();
  }

  // ---------------------------------------------------------------- layout (P2)

  function layout() {
    const W = stage.clientWidth;
    const H = stage.clientHeight;
    if (!W || !H) return;
    const s = { ...S.subs, display: shownDisplay() };
    const fs = Subs.fontSizes(s, H);
    root.style.setProperty('--pl-fs-en', `${fs.en}px`);
    root.style.setProperty('--pl-fs-zh', `${fs.zh}px`);
    const lines = Subs.reservedLines(s); // English only: no Chinese lines reserved
    const showSubs = s.display !== 'off';
    const padY = Math.round(Math.max(8, H * 0.016));
    const gap = lines.en && lines.zh ? Math.round(fs.zh * 0.22) : 0;
    root.style.setProperty('--pl-sub-gap', `${gap}px`);
    root.style.setProperty('--pl-sub-pad', `${padY}px`);
    // P2.1: the band reserves its largest size (2 + 2 lines), so it never jumps
    const bandH = showSubs ? Math.ceil(lines.en * fs.en * Subs.LH_EN + lines.zh * fs.zh * Subs.LH_ZH + gap + padY * 2) : 0;
    let overlay = false;
    let fr;
    if (S.kind === 'audio') {
      fr = { left: 0, top: 0, width: W, height: Math.max(0, H - bandH) };
    } else {
      const aspect = angles.aspect() || (media && media.aspect()) || 16 / 9;
      const fullH = Math.min(H, W / aspect);
      const bandPicH = Math.min(H - bandH, W / aspect);
      // P2.3: not enough height below the picture → overlay
      overlay = showSubs && (s.placement === 'overlay' || (s.placement === 'auto' && (bandPicH < AUTO_OVERLAY_RATIO * fullH || bandPicH < Math.min(AUTO_OVERLAY_MIN_PX, fullH))));
      const picH = Math.max(0, overlay || !showSubs ? fullH : bandPicH);
      const picW = Math.min(W, picH * aspect);
      const groupH = picH + (overlay || !showSubs ? 0 : bandH);
      fr = { left: Math.round((W - picW) / 2), top: Math.max(0, Math.round((H - groupH) / 2)), width: Math.round(picW), height: Math.round(picH) };
    }
    Object.assign(frame.style, { left: `${fr.left}px`, top: `${fr.top}px`, width: `${fr.width}px`, height: `${fr.height}px` });
    const changed = overlay !== S.overlay;
    S.overlay = overlay;
    root.classList.toggle('is-overlay', overlay);
    sub.el.classList.toggle('is-band', !overlay);
    sub.el.classList.toggle('is-overlay', overlay);
    sub.el.hidden = !showSubs;
    if (overlay) {
      const y = s.overlayY != null ? s.overlayY : 0.05;
      Object.assign(sub.el.style, {
        left: `${fr.left + fr.width * 0.04}px`,
        width: `${fr.width * 0.92}px`,
        top: '',
        height: '',
        bottom: `${H - (fr.top + fr.height) + fr.height * y}px`,
      });
    } else {
      Object.assign(sub.el.style, { left: '0px', width: `${W}px`, top: `${S.kind === 'audio' ? H - bandH : fr.top + fr.height}px`, height: `${bandH}px`, bottom: '' });
    }
    root.style.setProperty('--pl-ctl-h', `${controls.offsetHeight}px`);
    root.style.setProperty('--pl-stage-h', `${H}px`);
    root.style.setProperty('--pl-band-h', `${overlay || !showSubs ? 0 : bandH}px`);
    angles.layout(fr, { W, H, overlay });
    sub.refit();
    if (changed) ccPanel.refresh();
  }

  const ro = new ResizeObserver(() => layout());
  ro.observe(stage);

  // P2.3: in overlay mode the subtitles can be dragged up and down; the
  // position is remembered (localStorage, with the other subtitle settings).
  sub.el.addEventListener('pointerdown', (e) => {
    if (!S.overlay || e.button !== 0) return;
    e.preventDefault();
    const fr = frame.getBoundingClientRect();
    const start = e.clientY;
    const startBottom = fr.bottom - sub.el.getBoundingClientRect().bottom;
    sub.el.setPointerCapture(e.pointerId);
    sub.el.classList.add('is-dragging');
    const move = (ev) => {
      const bottom = startBottom - (ev.clientY - start);
      S.subs.overlayY = Math.max(0, Math.min(0.85, bottom / Math.max(1, fr.height)));
      layout();
    };
    const up = () => {
      sub.el.removeEventListener('pointermove', move);
      sub.el.removeEventListener('pointerup', up);
      sub.el.removeEventListener('pointercancel', up);
      sub.el.classList.remove('is-dragging');
      setSubs({ overlayY: S.subs.overlayY });
    };
    sub.el.addEventListener('pointermove', move);
    sub.el.addEventListener('pointerup', up);
    sub.el.addEventListener('pointercancel', up);
  });

  // ---------------------------------------------------------------- modes (P2.2, P2.3)

  function sideMode() {
    if (S.fs) return S.fsSide ? 'column' : S.sideOverlay ? 'overlay' : 'hidden';
    if (S.narrow || S.theater) return S.sideOverlay ? 'overlay' : 'hidden';
    return 'column';
  }

  function applyModes() {
    const before = root.dataset.side;
    const mode = sideMode();
    root.dataset.side = mode;
    root.classList.toggle('is-theater', S.theater || S.narrow);
    root.classList.toggle('is-narrow', S.narrow);
    root.classList.toggle('is-fs', S.fs);
    theaterBtn.setAttribute('aria-pressed', String(S.theater));
    theaterBtn.classList.toggle('is-on', S.theater);
    sideBtn.setAttribute('aria-pressed', String(mode !== 'hidden'));
    sideBtn.classList.toggle('is-on', mode === 'overlay');
    sideBtn.hidden = mode === 'column';
    clear(fsBtn).append(pIcon(S.fs ? 'minimize' : 'maximize', { size: 18 }));
    const fsName = S.fs ? t('player.ctl.exit_fullscreen') : t('player.ctl.fullscreen');
    fsBtn.setAttribute('aria-label', fsName);
    fsBtn.title = tip(fsName, 'F');
    if (mode !== 'hidden' && before === 'hidden') transcript.reveal();
    layout();
  }

  function toggleTheater() {
    if (S.fs) {
      S.fsSide = !S.fsSide;
      S.sideOverlay = false;
    } else if (S.narrow) {
      S.sideOverlay = !S.sideOverlay;
    } else {
      S.theater = !S.theater;
      S.sideOverlay = false;
      writePref(PREF_THEATER, S.theater);
    }
    applyModes();
  }

  function toggleSideOverlay(on) {
    if (sideMode() === 'column') return;
    S.sideOverlay = on == null ? !S.sideOverlay : on;
    applyModes();
    if (!S.sideOverlay) sideBtn.focus({ preventScroll: true });
  }

  function toggleFullscreen() {
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    else if (root.requestFullscreen) root.requestFullscreen().catch(() => toast(t('player.fullscreen_failed'), 'warn'));
  }

  // Menus (appended to <body>) and toasts would be invisible outside the
  // full-screen element: while in full screen they are moved into it.
  let fsObserver = null;
  function onFullscreen() {
    S.fs = document.fullscreenElement === root;
    S.fsSide = false;
    S.sideOverlay = false;
    const toasts = document.getElementById('toasts');
    if (S.fs) {
      if (toasts) root.append(toasts);
      fsObserver = new MutationObserver((recs) => {
        for (const r of recs)
          for (const n of r.addedNodes) {
            if (!n.classList || !n.classList.contains('ctx-menu')) continue;
            // menu.js focuses the first item while the menu is still under
            // <body>, which does not take in full screen: focus it here
            const a = document.activeElement;
            root.append(n);
            const item = a && n.contains(a) ? a : n.dataset.level === '0' ? n.querySelector('.ctx-item') : null;
            if (item) item.focus({ preventScroll: true });
          }
      });
      fsObserver.observe(document.body, { childList: true });
    } else {
      if (toasts && toasts.parentNode === root) document.body.append(toasts);
      if (fsObserver) fsObserver.disconnect();
      fsObserver = null;
    }
    applyModes();
  }
  document.addEventListener('fullscreenchange', onFullscreen);

  function onResize() {
    const narrow = window.innerWidth < NARROW_BELOW;
    if (narrow !== S.narrow) {
      S.narrow = narrow;
      S.sideOverlay = false; // P2.2: back to the user's own mode
      applyModes();
    }
  }
  window.addEventListener('resize', onResize);
  const onKinds = () => alive && renderHead(); // kind names (lecture kinds)
  window.addEventListener('kindschange', onKinds);

  // A click on the picture plays / pauses; a double click toggles full screen.
  frame.addEventListener('click', (e) => {
    if (S.kind === 'video' && e.target === angles.pictureEl()) togglePlay();
  });
  frame.addEventListener('dblclick', (e) => {
    if (S.kind === 'video' && e.target === angles.pictureEl()) toggleFullscreen();
  });

  // The second angle was swapped, shown, hidden or lost: the picture's
  // aspect may change, and the clock's frame loop depends on whether the
  // main video is on screen.
  function onAnglesChanged() {
    if (!alive) return;
    if (media && S.kind === 'video') {
      startFrameLoop();
      if (useRaf && !media.paused && !raf) raf = requestAnimationFrame(rafLoop);
    }
    layout();
  }

  // ---------------------------------------------------------------- controls

  function rateMenu() {
    openMenu(
      // T23 general 17: a radio menu (menuitemradio, aria-checked), as the language menu
      RATES.map((r) => ({ label: `${fmtDecimal(r)}×`, checked: r === S.rate, onSelect: () => setRate(r) })),
      { anchor: rateBtn },
      { returnFocus: rateBtn, focus: Math.max(0, RATES.indexOf(S.rate)) },
    );
  }

  function setRate(r, quiet = false) {
    S.rate = r;
    if (media) media.setRate(r);
    writePref(PREF_RATE, r);
    rateBtn.textContent = `${fmtDecimal(r)}×`;
    // T23 general 17: the name says the speed too (it replaced the text "1.5×")
    rateBtn.setAttribute('aria-label', t('player.ctl.rate_now', { rate: `${fmtDecimal(r)}×` }));
    rateBtn.classList.toggle('is-on', r !== 1);
    if (!quiet) osd(t('player.osd.rate', { rate: fmtDecimal(r) }));
  }

  function stepRate(dir) {
    const i = RATES.indexOf(S.rate);
    const n = RATES[Math.max(0, Math.min(RATES.length - 1, (i < 0 ? 2 : i) + dir))];
    if (n !== S.rate) setRate(n);
    else osd(t('player.osd.rate', { rate: fmtDecimal(n) }));
  }

  function setVolume(v, muted) {
    S.volume = Math.max(0, Math.min(1, v));
    S.muted = muted || S.volume === 0;
    if (media) media.setVolume(S.volume, S.muted);
    writePref(PREF_VOLUME, { v: S.volume, m: S.muted });
    volRange.value = String(S.muted ? 0 : S.volume);
    clear(volBtn).append(pIcon(S.muted ? 'volume-x' : S.volume < 0.5 ? 'volume-1' : 'volume-2', { size: 18 }));
    const name = S.muted ? t('player.ctl.unmute') : t('player.ctl.mute');
    volBtn.setAttribute('aria-label', name);
    volBtn.title = name;
  }

  function osd(text) {
    osdEl.textContent = text;
    osdEl.classList.add('is-on');
    clearTimeout(osdTimer);
    osdTimer = setTimeout(() => osdEl.classList.remove('is-on'), 1100);
  }

  // ---------------------------------------------------------------- head, notices

  function coursePrefix() {
    const c = courseForCite();
    return c ? coursePart(c) : '';
  }

  // P13.6: "{label} {English title}"; the course code before it, quieter.
  // lecture kinds: the kind and week replace the number ("Week 2 · Tutorial");
  // the citation number is in the hover text.
  function renderHead() {
    if (!S.lec) return;
    // T28 suggestion 8 (round 9, with the dashboard line's title): the
    // shell names the window after the current course as on the lecture
    // page, which says which course it is on every draw (view_lecture.js);
    // opened straight from the dashboard the player was in none. The name
    // comes from the shell's course list: no request here.
    ctx.setCurrentCourse(S.lec.course_id);
    courseEl.textContent = coursePrefix();
    courseEl.hidden = !courseEl.textContent;
    refEl.textContent = lectureTagText(S.lec);
    refEl.title = t('lecture.cite_tip', { ref: `${coursePart(courseForCite())} ${S.lec.cite_label || S.lec.label}` });
    nameEl.textContent = zh(S.lec.title); // P14.6: Chinese typed into the title is shown in the display script
    nameEl.title = zh(S.lec.title);
  }

  function renderChapterNow() {
    const list = chapters.chapters();
    const c = S.curChapter >= 0 ? list[S.curChapter] : null;
    clear(chapterNow);
    if (c) chapterNow.append(h('span', { class: 'pl-chapter-now-text', lang: chapters.titleLang(c) }, chapters.title(c)), pIcon('chevron-down', { size: 14 }));
    chapterNow.hidden = !c;
  }

  // ext web3: the chapters in the transcript stream (headings), in the
  // display's language.
  function syncTranscriptChapters() {
    transcript.setChapters(chapters.chapters(), { title: (c) => chapters.title(c), lang: (c) => chapters.titleLang(c), onSeek: (ms) => seekTo(ms) });
  }

  function showNotice(ms) {
    clearTimeout(noticeTimer);
    clear(notice).append(
      pIcon('rotate-ccw', { size: 15 }),
      h('span', null, t('player.resume.from', { time: fmtPlayerTime(ms) })),
      h(
        'button',
        {
          type: 'button',
          class: 'pl-notice-btn',
          onclick: () => {
            seekTo(0);
            hideNotice();
          },
        },
        t('player.resume.restart'),
      ),
      h('button', { type: 'button', class: 'pl-notice-x', 'aria-label': t('common.close'), title: t('common.close'), onclick: hideNotice }, pIcon('x', { size: 14 })),
    );
    notice.hidden = false;
    noticeTimer = setTimeout(hideNotice, 12000);
  }

  function hideNotice() {
    clearTimeout(noticeTimer);
    notice.hidden = true;
  }

  function showStageMsg(text, onRetry) {
    clear(stageMsg).append(h('p', null, text), onRetry ? h('button', { type: 'button', class: 'btn btn-sm', onclick: onRetry }, t('common.retry')) : null);
    stageMsg.hidden = false;
  }

  function hideStageMsg() {
    stageMsg.hidden = true;
  }

  // ---------------------------------------------------------------- audio mode (P8.1)

  function streamStatus() {
    return (S.lec && S.lec.stream && S.lec.stream.status) || '';
  }

  function audioStatus() {
    const st = S.lec && S.lec.stream;
    switch (st ? st.status : '') {
      case 'ready':
        return S.audioWhy === 'video_failed' ? { text: t('player.audio.video_failed') } : { text: t('player.audio.stream_ready'), switchable: true };
      case 'generating':
        return { text: t('player.audio.generating', { pct: Math.round((st.progress || 0) * 100) }), progress: st.progress || 0 };
      case 'pending':
        return { text: t('player.audio.pending'), progress: 0 };
      case 'audio_only':
        return { text: t('player.audio.audio_only') };
      case 'no_space':
        return { text: t('player.audio.no_space') };
      case 'failed':
        return { text: t('player.audio.failed', { error: codeText(st.error) }), error: true };
      default:
        return { text: t('player.audio.unavailable') };
    }
  }

  function renderAudioPanel() {
    if (S.kind !== 'audio' || !S.lec) return;
    const st = audioStatus();
    const list = chapterList(chapters, { current: S.curChapter, onSeek: (ms) => seekTo(ms), duration: duration() });
    const panel = h(
      'div',
      { class: 'pl-audio' },
      h('div', { class: 'pl-audio-mode' }, pIcon('headphones', { size: 16 }), h('span', null, t('player.audio.mode'))),
      h('h2', { class: 'pl-audio-title' }, coursePrefix() ? h('span', { class: 'pl-audio-ref' }, coursePrefix()) : null, h('span', null, `${lectureTagText(S.lec)} ${zh(S.lec.title)}`)),
      h(
        'div',
        { class: 'pl-audio-status' + (st.error ? ' is-err' : '') },
        h('span', null, st.text),
        st.progress != null ? h('span', { class: 'pl-audio-bar', 'aria-hidden': 'true' }, h('span', { style: { width: `${Math.round(st.progress * 100)}%` } })) : null,
        st.switchable
          ? h('button', { type: 'button', class: 'btn btn-sm btn-primary', onclick: () => setupMedia('video', S.t, S.playing) }, pIcon('video', { size: 14 }), h('span', { class: 'btn-label' }, t('player.audio.switch_to_video')))
          : null,
      ),
      list ? h('div', { class: 'pl-audio-ch-wrap' }, h('h3', { class: 'pl-audio-ch-head' }, t('chapters.button')), list) : h('p', { class: 'pl-audio-hint' }, chapters.running() ? t('chapters.generating') : t('player.audio.no_chapters')),
    );
    const scroller = frame.querySelector('.pl-audio-ch-wrap');
    const keep = scroller ? scroller.scrollTop : 0;
    clear(frame).append(panel);
    const again = frame.querySelector('.pl-audio-ch-wrap');
    if (again) again.scrollTop = keep;
  }

  // ---------------------------------------------------------------- data

  async function loadAllSentences() {
    const out = [];
    for (let offset = 0; ; ) {
      const page = await api.sentences(id, offset, 1000);
      const got = (page && page.sentences) || [];
      out.push(...got);
      offset += got.length;
      if (!got.length || offset >= page.total) break;
    }
    return out;
  }

  async function loadText(first) {
    const my = ++textSeq;
    if (first) transcript.setLoading();
    try {
      const [cues, sentences] = await Promise.all([api.cues(id, { quiet: !first }), loadAllSentences()]);
      if (!alive || my !== textSeq) return;
      for (const s of sentences) retranslated.seen(s); // T25 S3: the same translation back is said
      S.cues = cues || [];
      S.sentences = sentences;
      transcript.setSentences(sentences);
      S.curSent = -2;
      S.curCue = -2;
      sub.reset();
      tick(S.scrub != null ? S.scrub : S.t);
      transcript.setCurrent(S.curSent, { force: first });
      updateMarks();
      timeline.setDuration(duration());
      if (S.loop) {
        const s = S.sentences[S.loop.idx];
        if (s) S.loop = { idx: S.loop.idx, start: s.start_ms, end: s.end_ms };
        else setLoop(null);
      }
    } catch (e) {
      if (!alive || my !== textSeq) return;
      if (first || !S.sentences.length) transcript.setError(e.message, () => loadText(true));
    }
  }

  function scheduleText(ms) {
    debounce('text', () => loadText(false), ms);
  }

  function updateMarks() {
    timeline.setMarks({
      chapters: chapters.chapters(),
      sentences: S.sentences.map((s) => ({ start_ms: s.start_ms, modified: isModified(s), pending: isPending(s) })),
    });
  }

  function onChaptersChanged() {
    if (!alive) return;
    S.curChapter = -2;
    updateMarks();
    syncTranscriptChapters();
    tick(S.t);
  }

  async function reloadLecture() {
    try {
      const l = await api.lecture(id, { quiet: true });
      if (!alive || !l) return;
      const wasStream = streamStatus();
      const wasEn = enOnly();
      S.lec = l;
      renderHead();
      syncEnglishOnly(wasEn);
      angles.setAngles(lectureAngles(l));
      if (S.kind === 'audio') {
        renderAudioPanel();
        if (wasStream !== 'ready' && streamStatus() === 'ready') toast(t('player.audio.stream_ready_toast'), 'success');
      }
    } catch (e) {
      if (alive && e.status === 404) showFatal('gone');
    }
  }

  async function reloadCourse() {
    if (!S.lec) return;
    try {
      S.course = await api.course(S.lec.course_id, { quiet: true });
      renderHead();
      if (S.kind === 'audio') renderAudioPanel();
    } catch {
      // the citation falls back to the course list of the shell
    }
  }

  function debounce(name, fn, ms) {
    clearTimeout(timers.get(name));
    timers.set(
      name,
      setTimeout(() => {
        if (alive) fn();
      }, ms),
    );
  }

  // ---------------------------------------------------------------- resume (P6.4)

  async function startPosition() {
    if (startT != null) return { ms: startT, notice: false };
    try {
      const r = await api.resume(id, { quiet: true });
      const p = r ? r.position_ms : 0;
      const d = totalMs();
      if (p > RESUME_MIN_MS && (!d || p < d - RESUME_END_MARGIN_MS)) return { ms: p, notice: true };
    } catch {
      // nothing saved or not reachable: from the start
    }
    return { ms: 0, notice: false };
  }

  function saveResume(keepalive) {
    if (!S.ready || !media || !S.lec) return;
    const ms = Math.round(S.t);
    if (Math.abs(ms - lastSaved) < 500) return;
    lastSaved = ms;
    const p = api.saveResume(id, ms, { keepalive });
    if (p && p.catch) p.catch(() => {});
  }

  const saveTimer = setInterval(() => {
    if (S.playing) saveResume(false);
  }, SAVE_EVERY_MS);
  const onPageHide = () => saveResume(true);
  const onVisibility = () => {
    if (document.visibilityState === 'hidden') saveResume(true);
  };
  window.addEventListener('pagehide', onPageHide);
  document.addEventListener('visibilitychange', onVisibility);

  // ---------------------------------------------------------------- keys (P6.3)

  const unbindKeys = bindKeys(root, {
    toggle: togglePlay,
    seekBy,
    prev: prevSentence,
    next: nextSentence,
    loop: toggleLoop,
    rate: stepRate,
    display: cycleDisplay,
    theater: toggleTheater,
    fullscreen: toggleFullscreen,
    cite: () => copyCite(),
    search: () => {
      if (sideMode() === 'hidden') {
        S.sideOverlay = true;
        applyModes();
      }
      transcript.focusSearch();
    },
    help: () => openHelp({ angles: angles.available() }),
    angle: () => angles.swap(),
  });
  root.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && S.sideOverlay && !document.querySelector('dialog[open], .ctx-menu')) toggleSideOverlay(false);
  });

  // ---------------------------------------------------------------- boot

  function showFatal(kind, message = '') {
    teardownMedia();
    if (document.fullscreenElement === root) document.exitFullscreen().catch(() => {});
    clear(root).append(
      h(
        'div',
        { class: 'pl-fatal' },
        pIcon(kind === 'not_playable' ? 'clock' : 'circle-alert', { size: 28 }),
        h('h1', null, kind === 'not_playable' ? t('player.not_playable') : kind === 'error' ? t('player.load_failed') : t('player.not_found')),
        message ? h('p', { class: 'pl-fatal-msg' }, message) : null,
        kind === 'not_playable' ? h('p', { class: 'pl-fatal-msg' }, t('player.not_playable_hint')) : null,
        h(
          'div',
          { class: 'pl-fatal-actions' },
          kind === 'error' ? h('button', { type: 'button', class: 'btn btn-primary', onclick: () => location.reload() }, t('common.retry')) : null,
          kind === 'not_playable' ? h('a', { class: 'btn btn-primary', href: `#/lecture/${enc(id)}` }, t('player.back')) : null,
          h('a', { class: 'btn', href: '#/' }, t('player.to_dashboard')),
        ),
      ),
    );
  }

  async function boot() {
    root.dataset.side = sideMode();
    root.dataset.order = S.subs.order;
    root.style.setProperty('--pl-ov-alpha', String(S.subs.opacity));
    transcript.setDisplay(shownDisplay(), S.subs.order);
    setRate(S.rate, true);
    setVolume(S.volume, S.muted);
    applyModes();
    transcript.setLoading();
    spinner.hidden = false;
    const autoplay = !!(navigator.userActivation && navigator.userActivation.isActive);
    let lec;
    try {
      lec = await api.lecture(id, { quiet: true });
    } catch (e) {
      if (!alive) return;
      showFatal(e.status === 404 ? 'gone' : 'error', e.status === 404 ? '' : e.message);
      return;
    }
    if (!alive) return;
    const wasEn = enOnly();
    S.lec = lec;
    renderHead();
    syncEnglishOnly(wasEn);
    angles.setAngles(lectureAngles(lec));
    if (!lec.playable) {
      showFatal('not_playable');
      return;
    }
    if (!S.course) reloadCourse();
    const textP = loadText(true);
    const pos = await startPosition();
    if (!alive) return;
    const st = streamStatus();
    if (st === 'pending') {
      // P8.1: the first request for /media starts the background generation
      fetch(api.mediaURL(id), { headers: { Range: 'bytes=0-0' }, cache: 'no-store' })
        .catch(() => {})
        .finally(() => debounce('lec', reloadLecture, 400));
    }
    setupMedia(st === 'ready' ? 'video' : 'audio', pos.ms, autoplay);
    if (pos.notice) showNotice(pos.ms);
    await textP;
  }

  boot();

  // ---------------------------------------------------------------- view interface (app.js)

  return {
    // #/play/{id}?t=… for the same lecture: seek without re-mounting
    update(r) {
      if (r.name !== 'play' || r.id !== id) return false;
      const at = routeTime(r);
      if (at != null) seekTo(at);
      return true;
    },
    onChanged(ev) {
      if (!S.lec) return;
      if (ev.kind === 'course' && ev.id === S.lec.course_id) debounce('course', reloadCourse, 300);
      if (ev.id !== id) return;
      if (ev.kind === 'sentences' || ev.kind === 'patches') scheduleText(500); // P8.2: refetch, keep the position
      else if (ev.kind === 'lecture') debounce('lec', reloadLecture, 300);
      else if (ev.kind === 'chapters') chapters.load();
    },
    onJob(ev) {
      if (ev.scope !== 'lecture' || ev.scope_id !== id) return;
      if (ev.status === 'running') {
        // the background generation of stream.mp4 (an auxiliary task) reports progress
        if (ev.stage === 'stream' && S.lec) {
          S.lec.stream = { ...(S.lec.stream || {}), status: 'generating', progress: ev.progress };
          if (S.kind === 'audio') debounce('panel', renderAudioPanel, 250);
        }
        return;
      }
      debounce('lec', reloadLecture, 400);
      if (ev.stage === 'chapters') chapters.load();
      if ((ev.stage === 'repair' || ev.stage === 'translate') && ev.status === 'done') scheduleText(800);
    },
    refresh() {
      reloadLecture();
      loadText(false);
      chapters.load();
    },
    destroy() {
      saveResume(true);
      alive = false;
      closeMenu({ focus: false });
      ccPanel.close();
      if (document.fullscreenElement === root) document.exitFullscreen().catch(() => {});
      const toasts = document.getElementById('toasts');
      if (toasts && toasts.parentNode === root) document.body.append(toasts);
      if (fsObserver) fsObserver.disconnect();
      teardownMedia();
      angles.destroy();
      unbindKeys();
      ro.disconnect();
      timeline.destroy();
      transcript.destroy();
      clearInterval(saveTimer);
      clearTimeout(waitTimer);
      clearTimeout(noticeTimer);
      clearTimeout(osdTimer);
      for (const tm of timers.values()) clearTimeout(tm);
      document.removeEventListener('fullscreenchange', onFullscreen);
      window.removeEventListener('resize', onResize);
      window.removeEventListener('kindschange', onKinds);
      window.removeEventListener('pagehide', onPageHide);
      document.removeEventListener('visibilitychange', onVisibility);
      clear(main);
    },
  };
}
