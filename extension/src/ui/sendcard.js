// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// The send card (SPEC-ext 界面设计): the popup's content, and the card the
// in-page "发送至 LectureFlow" button opens on a platform's page. One
// implementation, two hosts: the popup (an extension page) and a content
// script's closed shadow DOM (src/content/inpage.js, bundled) — and the
// extension's confirmation window (src/confirm/), which says what the
// in-page card says.
//
// Views (src/lib/popupstate.js decides which): no tracks, LectureFlow not
// connected, ready to send, cannot send (imported, download switch off,
// protected), queued — plus "续传" for a needs_refresh task and the
// do-not-handle notice. Ticks follow src/lib/send.js. The card names tracks
// by id only; the background resolves them, reads cookies and sends.
//
// ask(cmd, extra) talks to the background:
//   detect {force} → { ok, view, origins, offers }
//   connection {recheck} → { ok, connection: { status, appVersion, courses, lastCourseId, os, reason, params } }
//   check → { ok, item, status }
//   send {trackIds, courseId, overrideDisabled, rememberPlatform} → { ok, taskId, position } | { ok: false, error, item }
//   refresh {trackIds} → { ok, taskId, state }
//   open {lectureId | taskId} → { ok }
//   status {taskId} → { ok, status }

import { el, icon, t, formatBytes } from './dom.js';
import { popupView, progressOf, sendSticks } from '../lib/popupstate.js';
import { tracksFoundKey } from '../lib/i18n.js';
import { defaultSelection, toggleSelection, selectionError, sendable, trackNames, presetSelection, PRESETS, shownTracks } from '../lib/send.js';
import { normalizeHost } from '../lib/blocklist.js';
import { parseHttpUrl } from '../lib/url.js';

/**
 * Why LectureFlow does not handle a site, by its category on the
 * do-not-handle list (src/lib/blocklist.js): YouTube, or online course
 * platforms; the general sentence for anything else.
 */
export function blockedText(category) {
  if (category === 'youtube') return t('unsupportedYoutube');
  if (category === 'courses') return t('unsupportedCourses');
  return t('unsupportedText');
}

/**
 * Why LectureFlow is not connected, from the connection view:
 *   not_open     installed, not running, and this hello asked not to start
 *                it (noLaunch; the host answers so only when it knows how
 *                to start it)
 *   not_started  the host tried to start it and could not, or it did not
 *                answer in time
 *   protocol     LectureFlow and the extension do not match
 *   not_found    no host (not installed, or the browser's registration is
 *                broken: Chrome says the same for both), or no way to start it
 */
export function disconnectedWhy(conn) {
  const c = conn || {};
  const sub = c.params && c.params.reason;
  if (c.reason === 'app.not_running' && sub === 'not_running') return 'not_open';
  if (c.reason === 'app.not_running' && (sub === 'start_failed' || sub === 'timeout')) return 'not_started';
  if (c.reason === 'protocol.unsupported') return 'protocol';
  return 'not_found';
}

/** What the "未连接" view says, by disconnectedWhy. */
export function disconnectedText(conn) {
  switch (disconnectedWhy(conn)) {
    case 'not_open':
      return t('discNotOpen');
    case 'not_started':
      return t('discNotRunning');
    case 'protocol':
      return t('discProtocol');
    default:
      return t('discNotInstalled');
  }
}

/**
 * Whether the send block sticks to the bottom of scroller (src/lib/
 * popupstate.js sendSticks for areaHeight, the visible height of what
 * scrolls), written on scroller as data-send-sticky for ui/card.css, with
 * the block's height as --send-h, the scroll padding that keeps a focused
 * control out from under it (SEC42 中-1, 中-2). The block is looked for in
 * within (the scroller itself, or the popup's document). Returns whether.
 */
export function fitSend(scroller, areaHeight, within = scroller) {
  const send = within.querySelector('.send');
  const h = send ? send.getBoundingClientRect().height : 0;
  const on = sendSticks(areaHeight, h);
  scroller.dataset.sendSticky = String(on);
  scroller.style.setProperty('--send-h', on ? `${Math.ceil(h)}px` : '0px');
  return on;
}

/**
 * Whether to point at LectureFlow's "重新登记" (discRegister): only when the
 * browser could not reach the host at all (not registered, or the program
 * it names is gone: Chrome says the same for both). When the host answered
 * (not_installed: no launch.json; a timeout) the registration is fine and
 * the diagnostics page cannot help (SEC42 低-3).
 */
export function registerMayHelp(conn) {
  return !!conn && conn.reason === 'ext.host_unavailable' && !conn.error;
}

// Failure codes of a fetch that a retry in LectureFlow can fix and that
// have no sentence of their own here: LectureFlow shows the reason and its
// "重试" (internal/fetch/retryable.go failedRetry, retry: true).
const RETRYABLE_FAILURES = new Set([
  'downloads.http_status',
  'downloads.engine_failed',
  'downloads.tls_failed',
  'downloads.playlist_invalid',
  'downloads.proxy_unsupported',
  'downloads.merge_failed',
  'downloads.import_failed',
  'downloads.restarted',
  'downloads.credentials_rejected',
  'downloads.credentials_expired',
  'downloads.credentials_cleared',
]);

/**
 * Whether the platform refused LectureFlow's request as unauthorised (401 /
 * 403): downloads.credentials_rejected, LectureFlow's code for both (a
 * needs_refresh task), or downloads.http_status with such a status (not
 * what this LectureFlow sends for them, but its params carry the status).
 * Sent from a page that embeds the player, the cookies the platform wants
 * may be ones the extension cannot hand over (SEC46 低-3).
 */
export function refusedSignIn(error) {
  const code = error && error.code;
  if (code === 'downloads.credentials_rejected') return true;
  const status = Number(error && error.params && error.params.status);
  return code === 'downloads.http_status' && (status === 401 || status === 403);
}

/** Two sentences of one note: no space after a CJK full stop. */
function sentences(a, b) {
  return /[。！？]$/.test(a) ? `${a}${b}` : `${a} ${b}`;
}

/**
 * A note about a refused fetch (refusedSignIn) with "如果录播嵌在别的网站里，
 * 可以打开播放器自己的页面再发送一次" after it, when the lecture may have been
 * sent from a page that embeds it (EU-02).
 */
function withEmbeddedHint(text, error, embedded) {
  return embedded && refusedSignIn(error) ? sentences(text, t('failEmbeddedHint')) : text;
}

/**
 * Why a fetch failed, for the "已加入队列" view (T26 G3), split as
 * LectureFlow splits it (internal/fetch/retryable.go): a failure a retry can
 * fix says what to do and that LectureFlow retries it; one a retry cannot
 * fix (the recording itself) says why and what to do instead, never "重试".
 * A code this version does not know is kept, in brackets inside a whole
 * sentence; LectureFlow treats it as retryable. embedded: the lecture may
 * have been sent from a page that embeds it (true unless the card knows
 * better): a refused fetch then also says to send it from the player's own
 * page (EU-02).
 */
export function failedText(error, { embedded = true } = {}) {
  return withEmbeddedHint(failedSentence(error), error, embedded);
}

function failedSentence(error) {
  const code = (error && error.code) || '';
  switch (code) {
    case 'downloads.network':
      return t('failNetwork');
    case 'downloads.no_space':
    // out of space, the partial download kept for resuming (r8 F5)
    case 'downloads.no_space_kept':
    case 'downloads.write_failed':
      return t('failDisk');
    case 'downloads.engine_missing':
      return t('failEngineMissing');
    case 'downloads.drm_protected':
      return t('blockedProtected');
    case 'downloads.site_blocked':
      return t('errSiteBlocked');
    case 'downloads.playlist_unsupported':
      return t('failUnsupported');
    case 'downloads.local_address':
    case 'downloads.loopback_address':
    case 'downloads.unusual_host':
      return t('failUnsafeAddress');
    case 'downloads.angle_no_video':
      return t('failAngle');
    // over the size one fetch may hold (a live stream, a server that never
    // stops): the same download again, so no "重试" (C165b)
    case 'downloads.too_large':
      return t('failTooLarge');
    default:
      break;
  }
  if (code.startsWith('upload.media_')) return t('failNotMedia');
  if (!code || RETRYABLE_FAILURES.has(code)) return t('failedHint');
  return t('failedCode', [code]);
}

/** A message for an error code from the background or LectureFlow. */
export function errorText(error) {
  const code = (error && error.code) || '';
  const params = (error && error.params) || {};
  switch (code) {
    case 'ext.site_blocked':
      return blockedText(params.category);
    case 'downloads.site_blocked':
      return t('errSiteBlocked');
    case 'course.not_found':
      return t('errCourseGone');
    case 'ext.stale':
    // a ticked track the background no longer lists (the page was detected
    // again, and past RETAIN_LIMITS.tracksShown it fell out of the list)
    case 'ext.selection.unknown_track':
      return t('errStale');
    case 'ext.selection.too_many_audio':
    case 'ext.selection.too_many_video':
    case 'enqueue.too_many_tracks':
      return t('oneEach');
    case 'ext.selection.protected':
    case 'enqueue.protected':
      return t('errProtected');
    case 'ext.selection.empty':
    case 'enqueue.no_tracks':
      return t('selectHint');
    case 'ext.no_course':
      return `${t('courseNone')} ${t('courseCreate')}`;
    case 'app.not_running':
    case 'ext.host_unavailable':
    case 'ext.closed':
      return t('errNotRunning');
    case 'ext.timeout':
      return t('errTimeout');
    case 'refresh.not_found':
      return t('errRefreshGone');
    // "在 LectureFlow 中打开": the lecture was deleted, or the task removed
    case 'open.not_found':
      return t('errOpenGone');
    case 'ext.info_failed':
      return t('errInfoFailed', [String(params.status || '')]);
    case 'ext.confirm_in_window':
      return t('errConfirmWindow');
    case 'ext.confirm_gone':
      return t('confirmGone');
    case 'request.invalid':
      return t('errInvalid', [String(params.field || '')]);
    default:
      return t('errGeneric', [code || '?']);
  }
}

/**
 * "对这个平台（Panopto）不再提醒" in the reminder before sending a possibly
 * restricted recording (user decision 2026-10-27): only for a platform
 * profile, never for generic detection; unticked unless the user ticks it.
 */
export function rememberBox(name, checked, onChange) {
  const box = el('input', { type: 'checkbox', id: 'override-remember' });
  box.checked = !!checked;
  box.addEventListener('change', () => onChange(box.checked));
  return el('label', { class: 'check-line', id: 'override-remember-line' }, box, el('span', { text: t('restrictedRemember', [name]) }));
}

/**
 * The restricted notice: the user's own text. When the reminder is off for
 * this platform, the same text without its last sentence ("发送前会再提醒一
 * 次"), and a line saying so.
 */
export function restrictedNotice({ remembered, name, banner }) {
  if (!remembered) return [banner(t('restrictedNotice'))];
  return [banner(t('restrictedNoticeQuiet')), el('p', { class: 'small muted', id: 'remembered-note', text: t('restrictedRememberedNote', [name]) })];
}

/**
 * LectureFlow has no course yet (T26 G9): where to make one, and "重新检测"
 * (recheck) to see it here without reopening. The bridge cannot open
 * LectureFlow's course page (open takes a lecture or a task only).
 */
export function noCourse(recheck) {
  return el(
    'div',
    { class: 'note no-course', id: 'no-course' },
    el('p', { text: t('courseNone') }),
    el('p', { text: t('courseCreate') }),
    el('div', { class: 'btn-row' }, el('button', { class: 'btn btn-sm', id: 'course-recheck', type: 'button', onclick: recheck }, icon('refresh'), el('span', { text: t('connRecheck') }))),
  );
}

/**
 * A host name for the card, from a host or an address (a track's host as
 * the background gives it): the URL parser's host — lower case, punycode, no
 * port, user info, path, query or trailing dot (src/lib/blocklist.js
 * normalizeHost, the do-not-handle list's own clean-up) — or '' when there
 * is none, it is not a plain host name or it is longer than a DNS name.
 */
export function cleanHost(raw) {
  if (typeof raw !== 'string' || raw.length > 2048) return '';
  const s = raw.trim();
  const h = /^[a-z][a-z0-9+.-]*:\/\//i.test(s) ? (parseHttpUrl(s)?.hostname || '').replace(/\.+$/, '') : normalizeHost(s);
  if (!h || h.length > 253) return '';
  return /^(\[[0-9a-f:.]+\]|[a-z0-9_-]+(\.[a-z0-9_-]+)*)$/.test(h) ? h : '';
}

const ipLiteral = (h) => h.startsWith('[') || /^[0-9.]+$/.test(h);

/**
 * Whether two hosts are surely of one site: the same host, or one under the
 * other. Without a public suffix list, two names that only share a parent
 * (video.example.edu and www.example.edu, but also a.co.uk and b.co.uk)
 * are not taken for one site: the card names a track's host once too
 * often rather than once too few (AUD3 中-1: which site a track is on).
 */
export function sameSiteHost(a, b) {
  const x = cleanHost(a);
  const y = cleanHost(b);
  if (!x || !y) return false;
  if (x === y) return true;
  if (ipLiteral(x) || ipLiteral(y)) return false;
  return x.endsWith(`.${y}`) || y.endsWith(`.${x}`);
}

/**
 * "在 <主机> 上" for a track on another site than the page (EU-03; AUD3
 * 中-1): the host only, never its path or query. '' on the page's own site,
 * or when the track has no host (the background's view gives track.host).
 */
export function trackHostNote(track, pageHost) {
  const h = cleanHost(track && track.host);
  if (!h || sameSiteHost(h, pageHost)) return '';
  return t('trackOnHost', [h]);
}

function kindIcon(kind) {
  return kind === 'video' ? 'video' : kind === 'audio' ? 'headphones' : 'file-unknown';
}

/**
 * createSendCard({ mount, ask, host: 'popup' | 'inpage' | 'window', onView, onGrant, openUrl })
 * Returns { start, onStatus, redetect, state }.
 */
export function createSendCard({ mount, ask, host = 'popup', onView = () => {}, onGrant = null, openUrl = () => {} }) {
  const inpage = host === 'inpage' || host === 'window'; // "确认发送" and its data note
  const S = {
    detection: null,
    origins: [],
    cookieHosts: [], // hosts whose cookies a send reads, offered one by one (popup)
    offers: [],
    conn: null,
    checked: false,
    checkItem: null,
    task: null,
    status: null,
    override: false,
    confirming: false, // the reminder before sending a possibly restricted recording
    remembered: false, // the user turned that reminder off for this platform
    rememberTick: false, // "对这个平台不再提醒", ticked in the open reminder
    preset: '', // main | dual | audio | custom
    picked: false, // the user changed the choice (a preset or a tick)
    refreshChoice: false, // "续传" took the choice the lecture was sent with
    advanced: false, // the full track list ("选择轨道…")
    selection: [],
    courseId: '',
    busy: '',
    message: null,
  };
  const root = el('div', { class: 'lf-card' });
  root.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && S.confirming) {
      ev.stopPropagation();
      closeConfirm(false);
    }
  });
  mount.replaceChildren(root);

  const tracks = () => (S.detection && S.detection.status === 'ok' ? S.detection.tracks : []);
  // Tracks the background left out of the view (src/lib/retain.js
  // RETAIN_LIMITS.tracksShown): counted, not listed, not choosable.
  const hiddenCount = () => (S.detection && S.detection.status === 'ok' && Number.isInteger(S.detection.hiddenTracks) && S.detection.hiddenTracks > 0 ? S.detection.hiddenTracks : 0);
  const totalCount = () => tracks().length + hiddenCount();
  // embedded YouTube or course platform content the detection left out
  const blockedEmbeds = () => (S.detection && S.detection.status === 'ok' && Array.isArray(S.detection.blockedEmbeds) ? S.detection.blockedEmbeds : []);
  const view = () =>
    popupView({ detection: S.detection, connection: S.conn, checked: S.checked, checkItem: S.checkItem, task: S.task, override: S.override });
  /**
   * Whether the lecture may have been sent from a page that embeds it: the
   * tab's top-level page on another site than its tracks (EU-02). Known only
   * in the popup, whose view.host is the top-level page (an in-page card's
   * may be a frame's), and only when every track looked at (the ticked ones,
   * or all) has a host; otherwise it may be.
   */
  const mayBeEmbedded = () => {
    if (host !== 'popup') return true;
    const list = tracks();
    const ticked = list.filter((x) => S.selection.includes(x.id));
    const looked = ticked.length ? ticked : list;
    return looked.length === 0 || looked.some((x) => !cleanHost(x.host) || !sameSiteHost(x.host, S.detection.host));
  };
  const say = (text, tone = '') => {
    S.message = text ? { text, tone } : null;
    render();
  };

  // ---------------------------------------------------------- pieces

  const minutes = (sec) => (sec > 0 ? t('durationMinutes', [String(Math.max(1, Math.round(sec / 60)))]) : '');

  function lectureCard() {
    const d = S.detection;
    const lec = d.lecture || {};
    const title = lec.title ? el('h1', { class: 'lec-title', text: lec.title }) : el('h1', { class: 'lec-title generated', text: t('titleGenerated') });
    // the platform's name, not "Panopto 对策文件 1.1.1" (T26 G11): the
    // profile and its version are in the tooltip, for contributors
    const source = d.profile ? d.profile.name : t('sourceGeneric');
    const meta = [lec.recordedAt ? lec.recordedAt.replace('T', ' ') : '', minutes(lec.durationSec), source].filter(Boolean).join(' · ');
    const chips = el('div', { class: 'chips' });
    if (d.profile && !d.profile.reviewed) chips.append(el('span', { class: 'chip chip-idle', dataset: { flag: 'unreviewed' }, text: t('unreviewed') }));
    if (d.downloadPolicy === 'disabled') {
      chips.append(el('span', { class: 'chip chip-warn', dataset: { flag: 'disabled' }, text: S.override ? t('downloadDisabledOverridden') : t('downloadDisabled') }));
    }
    // the profile's regexes were stopped as too slow (src/lib/regexguard.js):
    // whether the recording is restricted could not be read
    if (d.policyStopped) chips.append(el('span', { class: 'chip chip-warn', dataset: { flag: 'policy-stopped' }, text: t('policyStopped') }));
    if (d.summary && d.summary.protected > 0) {
      chips.append(el('span', { class: 'chip chip-warn', dataset: { flag: 'protected' } }, icon('lock'), el('span', { text: t('protectedCount', [String(d.summary.protected)]) })));
    }
    const card = el('section', { class: 'card lecture', id: 'lecture' }, title, el('p', { class: 'lec-meta', title: d.profile ? t('profileSource', [d.profile.name, d.profile.version]) : null, text: meta }), chips);
    // A profile that reads the page's own response, which was not seen.
    if (d.observe && d.observe.declared && !d.observe.seen) {
      if (!d.authorized && onGrant && S.origins.length) {
        card.append(el('p', { class: 'hint', id: 'observe-hint' }, t('observeNeedsGrant'), el('button', { class: 'link-btn', onclick: onGrant, text: t('grantSite') })));
      } else if (d.authorized) card.append(el('p', { class: 'hint', id: 'observe-hint', text: t('observeNeedsReload') }));
    }
    return card;
  }

  function trackItem(tr, { selectable, name }) {
    const disabled = !selectable || !sendable(tr);
    const checked = S.selection.includes(tr.id);
    // the profile's hint (or the kind of a named track), its quality, and
    // the host when it is on another site than the page (EU-03); the format
    // words (HLS, DASH, 单文件) only in the tooltip (T26 G11)
    const sub = [tr.hint || (tr.label ? t(`kind_${tr.kind}`) : ''), tr.label && tr.quality ? tr.quality : '', trackHostNote(tr, S.detection.host)].filter(Boolean).join(' · ');
    const chips = [];
    if (tr.protected) chips.push(el('span', { class: 'chip chip-warn' }, icon('lock'), el('span', { text: t('protected') })));
    if (tr.recommended && !tr.protected) chips.push(el('span', { class: 'chip chip-run', text: t('recommended') }));
    const box = el('input', { type: 'checkbox', 'aria-label': t('selectTrack', [name]), dataset: { trackId: tr.id } });
    box.checked = checked;
    box.disabled = disabled || !!S.busy;
    box.addEventListener('change', () => onTick(tr, box));
    return el(
      'li',
      { class: 'track', dataset: { kind: tr.kind, format: tr.format, role: tr.role || '', protected: String(!!tr.protected), disabled: String(disabled), checked: String(checked) } },
      el(
        'label',
        { class: 'track-row', title: `${t(`kind_${tr.kind}`)} · ${t(`format_${tr.format}`)}` },
        box,
        el(
          'span',
          { class: 'track-text' },
          el('span', { class: 'track-line' }, el('span', { class: 'track-name', text: name }), ...chips),
          sub ? el('span', { class: 'track-sub', text: sub }) : null,
          // a playlist that could not be read, in words (T26 G11)
          tr.unreadable ? el('span', { class: 'track-sub track-warn', dataset: { flag: 'unreadable' }, text: t('unreadable') }) : null,
        ),
        el('span', { class: 'track-size', text: formatBytes(tr.sizeBytes, tr.sizeEstimated) }),
      ),
    );
  }

  const PRESET_KEYS = { main: 'presetMain', dual: 'presetDual', audio: 'presetAudio' };

  /**
   * What the current ticks are, in a line: "完整录像（MP4） · 216 MB". Nothing
   * when no choice can be made at all (protected tracks only: the banner
   * says why).
   */
  function chosenLine(selectable = true) {
    const picked = tracks().filter((x) => S.selection.includes(x.id));
    if (picked.length === 0) return selectable ? t('chooseContent') : '';
    const named = trackNames(tracks(), t);
    const names = picked.map((x) => named.get(x.id)).join(' + ');
    const sizes = picked.map((x) => x.sizeBytes);
    const total = sizes.every((n) => n > 0) ? formatBytes(sizes.reduce((x, y) => x + y, 0), picked.some((x) => x.sizeEstimated)) : '';
    return total ? `${names} · ${total}` : names;
  }

  /**
   * The three choices (user decision: no multiple choice): 主要音视频 /
   * 双机位 / 仅音频; "选择轨道…" opens the full list (advanced).
   */
  function trackSection({ selectable }) {
    const list = tracks();
    const presets = S.detection.presets || null;
    // the background sends at most RETAIN_LIMITS.tracksShown; the list is
    // held to that here too
    const shown = shownTracks(list, presets);
    const total = totalCount();
    const hidden = hiddenCount() + shown.hidden;
    const named = trackNames(list, t);
    const seg = el('div', { class: 'seg', id: 'presets', role: 'group', 'aria-label': t('contentLabel') });
    // why a choice cannot be made, on the card itself (T26 G4); not when no
    // choice can be made at all (protected tracks: the banner says why)
    const unavailable = [];
    for (const p of PRESETS) {
      const ids = presetSelection(list, p, presets);
      if (!ids && selectable) unavailable.push(t('presetUnavailable', [t(PRESET_KEYS[p]), t(`${PRESET_KEYS[p]}None`)]));
      const b = el('button', { type: 'button', dataset: { preset: p }, 'aria-pressed': String(S.preset === p), title: ids ? null : t(`${PRESET_KEYS[p]}None`), text: t(PRESET_KEYS[p]) });
      b.disabled = !selectable || !ids || !!S.busy;
      b.addEventListener('click', () => {
        S.preset = p;
        S.selection = ids;
        S.picked = true;
        render();
      });
      seg.append(b);
    }
    const toggle = el(
      'button',
      {
        class: 'link-btn',
        type: 'button',
        id: 'advanced',
        'aria-expanded': String(S.advanced),
        onclick: () => {
          S.advanced = !S.advanced;
          render();
        },
      },
      el('span', { text: S.advanced ? t('hideTracks') : t('chooseTracks', [String(total)]) }),
    );
    return el(
      'section',
      { class: 'sec' },
      el('div', { class: 'field-wrap' }, el('span', { class: 'field-label', text: t('contentLabel') }), seg),
      unavailable.length ? el('p', { class: 'small faint', id: 'presets-why', text: unavailable.join(' ') }) : null,
      el('div', { class: 'sec-head' }, el('span', { class: 'small muted', id: 'chosen', text: chosenLine(selectable) }), toggle),
      S.advanced
        ? el(
            'div',
            { class: 'sec', id: 'advanced-list' },
            el('div', { class: 'sec-head' }, el('h2', { id: 'count', dataset: { count: String(total) }, text: t(tracksFoundKey(total), [String(total)]) }), el('span', { class: 'sec-note', text: t('oneEach') })),
            el('ul', { class: 'tracks', id: 'tracks' }, shown.tracks.map((tr) => trackItem(tr, { selectable, name: named.get(tr.id) }))),
            hidden > 0 ? el('p', { class: 'small muted', id: 'tracks-hidden', dataset: { hidden: String(hidden) }, text: t('tracksHidden', [String(hidden)]) }) : null,
          )
        : null,
    );
  }

  function courseField(enabled) {
    const c = S.conn || {};
    const courses = c.courses || [];
    if (courses.length === 0) return noCourse(() => connect(true));
    const select = el(
      'select',
      { class: 'input', id: 'course', 'aria-label': t('courseLabel') },
      courses.map((x) => el('option', { value: String(x.id), text: String(x.id) === c.lastCourseId ? `${x.name}${t('courseLast')}` : x.name })),
    );
    select.value = S.courseId;
    select.disabled = !enabled || !!S.busy;
    select.addEventListener('change', () => {
      S.courseId = select.value;
    });
    return el('label', { class: 'field-inline' }, el('span', { class: 'field-label', text: t('courseLabel') }), select);
  }

  function primary(textKey, enabled, onclick) {
    const b = el('button', { class: 'btn btn-primary btn-block', id: 'primary', onclick }, el('span', { text: t(textKey) }));
    b.disabled = !enabled;
    return b;
  }

  function banner(text, tone, id, withIcon = false) {
    return el('p', { class: `note${tone ? ' note-' + tone : ''}`, id }, withIcon ? el('span', { class: 'note-icon' }, icon('alert-triangle')) : null, text);
  }

  const platformName = () => (S.detection && S.detection.profile ? S.detection.profile.name : '');
  const restrictedBanners = () => restrictedNotice({ remembered: S.remembered, name: platformName(), banner: (text) => banner(text, 'warn', 'reason', true) });

  function openButton(target) {
    return el('button', { class: 'btn', id: 'open', onclick: () => openInApp(target) }, el('span', { text: t('openInApp') }));
  }

  function stateBlock(iconName, titleText, ...rest) {
    return el('section', { class: 'card state' }, el('span', { class: 'state-icon' }, icon(iconName)), el('h1', { id: 'state-title', text: titleText }), ...rest);
  }

  function sendBlock({ enabled, textKey, onclick, withCourse }) {
    const err = selectionError(tracks(), S.selection, { maxVideo: 2 });
    const canSend = enabled && !err && !S.busy && (!withCourse || !!S.courseId);
    const busyKey = S.busy === 'refresh' ? 'refreshing' : 'sending';
    return el(
      'section',
      { class: 'send' },
      withCourse ? courseField(enabled) : null,
      primary(S.busy ? busyKey : textKey, canSend, onclick),
      el('p', { class: 'data-note', id: 'data-note', text: t(inpage ? 'inpageNote' : 'dataNote') }),
    );
  }

  // ------------------------------------------------------------ views

  // what the card waits for (src/lib/popupstate.js; T26 G7)
  const LOADING_TEXT = { launching: 'launchingApp', connecting: 'connectingApp', checking: 'connectingApp' };
  const renderLoading = (reason) => [el('section', { class: 'card state' }, el('p', { class: 'muted', id: 'state-title', text: t(LOADING_TEXT[reason] || 'detecting') }))];

  const renderUnsupported = (category) => [stateBlock('world', t('unsupportedTitle'), el('p', { class: 'muted', id: 'unsupported-text', dataset: { category: category || '' }, text: blockedText(category) }))];

  function renderNone(reason) {
    const why = reason === 'no_access' ? t('noAccess') : reason === 'not_a_page' ? t('notAPage') : reason === 'page_loading' ? t('pageLoading') : '';
    const offers = S.offers.map((o) =>
      el('p', { class: 'note', dataset: { offer: o.id } }, t(o.embedded ? 'offerEmbedded' : 'offerOpaque', [o.name]), ' ', onGrant ? el('button', { class: 'link-btn', onclick: () => onGrant(o.origins), text: t('offerGrant', [o.name]) }) : null),
    );
    return [
      stateBlock(
        'file-unknown',
        t('noTracks'),
        el('p', { class: 'muted', id: 'empty', text: t('emptyHint') }),
        why ? el('p', { class: 'small faint', text: why }) : null,
        blockedEmbeds().length ? el('p', { class: 'small muted', id: 'blocked-embed', dataset: { categories: blockedEmbeds().join(' ') }, text: t('blockedEmbedNote') }) : null,
        el('div', { class: 'btn-row' }, el('button', { class: 'btn', id: 'redetect', onclick: () => detect(true) }, icon('refresh'), el('span', { text: t('redetect') }))),
      ),
      ...offers,
    ];
  }

  function renderDisconnected() {
    const c = S.conn || {};
    const why = disconnectedWhy(c);
    const unsupportedOs = c.os === 'linux' || c.os === 'cros' || c.os === 'openbsd';
    const n = totalCount();
    // Installed but not running (the passive hello's answer): open it, or
    // look again without starting it (the user may have opened it; SPEC-ext
    // 界面设计: "未连接" comes with "重新检测").
    const buttons =
      why === 'not_open'
        ? [
            el('button', { class: 'btn', id: 'launch', onclick: () => connect(true) }, icon('plug-connected'), el('span', { text: t('openApp') })),
            el('button', { class: 'btn btn-ghost', id: 'recheck', onclick: () => connect(true, { passive: true }) }, icon('refresh'), el('span', { text: t('connRecheck') })),
          ]
        : [
            el('button', { class: 'btn', id: 'recheck', onclick: () => connect(true) }, icon('refresh'), el('span', { text: t('connRecheck') })),
            el('button', { class: 'btn btn-ghost', id: 'get-app', onclick: () => openUrl('https://github.com/ShouldBeQuincy/LectureFlow/releases/latest') }, icon('download'), el('span', { text: t('getLectureFlow') })),
          ];
    return [
      el('p', { class: 'summary-line', id: 'count', dataset: { count: String(n) }, text: t(tracksFoundKey(n), [String(n)]) }),
      stateBlock(
        'plug-connected-x',
        t('disconnectedTitle'),
        el('p', { class: 'muted', id: 'disc-text', dataset: { why }, text: disconnectedText(c) }),
        registerMayHelp(c) ? el('p', { class: 'small muted', id: 'disc-register', text: t('discRegister') }) : null,
        unsupportedOs ? el('p', { class: 'note note-warn', id: 'os-note', text: t('discUnsupportedOs') }) : null,
        el('div', { class: 'btn-row' }, ...buttons),
      ),
    ];
  }

  function renderReady(reason) {
    const out = [lectureCard()];
    if (reason === 'failed') out.push(banner(t('againFailed'), '', 'again'));
    else if (reason === 'cancelled') out.push(banner(t('againCancelled'), '', 'again'));
    if (S.detection.downloadPolicy === 'disabled') out.push(...restrictedBanners());
    out.push(trackSection({ selectable: true }));
    out.push(sendBlock({ enabled: true, textKey: inpage ? 'confirmSend' : 'sendButton', onclick: () => send(), withCourse: true }));
    return out;
  }

  function renderBlocked(reason) {
    const out = [lectureCard()];
    if (reason === 'imported') {
      out.push(banner(t('blockedImported'), 'ok', 'reason'));
      const lectureId = S.checkItem && S.checkItem.lectureId;
      out.push(el('div', { class: 'btn-row' }, openButton(lectureId ? { lectureId } : { taskId: S.checkItem && S.checkItem.taskId })));
      out.push(primary(inpage ? 'confirmSend' : 'sendButton', false, null));
      return out;
    }
    if (reason === 'disabled') {
      // Possibly restricted: nothing chosen at first; the user picks, and
      // is reminded once before it is sent (unless they turned that off
      // for this platform).
      out.push(...restrictedBanners());
      out.push(trackSection({ selectable: true }));
      out.push(sendBlock({ enabled: true, textKey: inpage ? 'confirmSend' : 'sendButton', onclick: () => send(), withCourse: true }));
      return out;
    }
    out.push(banner(t('blockedProtected'), 'warn', 'reason', true));
    out.push(trackSection({ selectable: false }));
    out.push(primary(inpage ? 'confirmSend' : 'sendButton', false, null));
    return out;
  }

  function renderRefresh() {
    // why it needs a refresh: the status pushed (kept on checkItem) or the
    // one the check brought
    const error = (S.checkItem && S.checkItem.error) || (S.status && S.status.error) || null;
    return [
      lectureCard(),
      banner(withEmbeddedHint(t('refreshText'), error, mayBeEmbedded()), 'warn', 'reason'),
      trackSection({ selectable: true }),
      sendBlock({ enabled: true, textKey: 'refreshButton', onclick: refresh, withCourse: false }),
    ];
  }

  function renderQueued() {
    const taskId = (S.task && S.task.taskId) || (S.checkItem && S.checkItem.taskId) || 0;
    const st = S.status || { state: (S.task && S.task.state) || (S.checkItem && S.checkItem.state) || 'queued', position: (S.task && S.task.position) || 0, bytesDone: 0, bytesTotal: -1 };
    const state = st.state || 'queued';
    const tone = { queued: 'run', downloading: 'run', merging: 'run', importing: 'run', done: 'ok', failed: 'bad', needs_refresh: 'warn', cancelled: 'idle' }[state] || 'run';
    const head = el('div', { class: 'status-head' }, el('span', { class: `chip chip-${tone}`, id: 'task-state', dataset: { state }, text: t(`state_${state}`) }));
    if (state === 'queued' && st.position > 0) head.append(el('span', { class: 'pos', id: 'position', text: t('position', [String(st.position)]) }));
    const p = progressOf(st);
    const fill = el('div', { class: 'bar-fill' });
    if (p.percent >= 0) fill.style.width = `${p.percent}%`;
    const bar = el('div', { class: 'bar', dataset: { indeterminate: String(p.percent < 0 && state !== 'queued' && state !== 'done') } }, fill);
    let detail = '';
    if (state === 'downloading' || state === 'merging' || state === 'importing') {
      detail = p.percent >= 0 ? t('progress', [String(p.percent), formatBytes(p.done), formatBytes(p.total)]) : p.done > 0 ? t('progressUnknown', [formatBytes(p.done)]) : '';
    }
    // an in-page card is a card on the page, not a window (T26 J2)
    let note = t(host === 'inpage' ? 'closeHintCard' : 'closeHint');
    if (state === 'done') note = t('doneHint');
    else if (state === 'failed') note = failedText(st.error, { embedded: mayBeEmbedded() });
    else if (state === 'cancelled') note = t('cancelledHint');
    const card = el(
      'section',
      { class: 'card status', id: 'status', dataset: { state } },
      // "已加入队列" while it is in LectureFlow's queue; then what came of it (T26 J2)
      el('h1', { class: 'lec-title', id: 'state-title', text: t({ done: 'doneTitle', failed: 'failedTitle', cancelled: 'state_cancelled' }[state] || 'queuedTitle') }),
      head,
      bar,
      detail ? el('p', { class: 'small muted', id: 'progress', text: detail }) : null,
      el('p', { class: 'small muted', id: 'status-note', text: note }),
    );
    const target = st.lectureId ? { lectureId: st.lectureId } : { taskId };
    return [lectureCard(), card, el('div', { class: 'btn-row' }, openButton(target))];
  }

  function overlay() {
    if (!S.confirming) return null;
    const dialog = el(
      'section',
      { class: 'card dialog', role: 'alertdialog', 'aria-modal': 'true', 'aria-labelledby': 'override-title' },
      el('h2', { class: 'dialog-title', id: 'override-title' }, el('span', { class: 'dialog-icon' }, icon('alert-triangle')), el('span', { text: t('restrictedTitle') })),
      el('p', { id: 'override-text', text: t('restrictedConfirm') }),
      platformName()
        ? rememberBox(platformName(), S.rememberTick, (on) => {
            S.rememberTick = on;
          })
        : null,
      el(
        'div',
        { class: 'dialog-actions' },
        el('button', { class: 'btn', id: 'override-cancel', onclick: () => closeConfirm(false) }, el('span', { text: t('cancel') })),
        el('button', { class: 'btn btn-warn', id: 'override-confirm', onclick: () => closeConfirm(true) }, el('span', { text: t('sendAnyway') })),
      ),
    );
    return el('div', { class: 'overlay', id: 'override' }, dialog);
  }

  /**
   * "续传" starts with the choice the lecture was sent with (双机位 needs
   * both pictures again), unless the user already chose; once.
   */
  function refreshChoice() {
    if (S.refreshChoice || S.picked || !S.detection || !S.detection.lastPreset) return;
    S.refreshChoice = true;
    const ids = presetSelection(tracks(), S.detection.lastPreset, S.detection.presets || null);
    if (!ids) return;
    S.preset = S.detection.lastPreset;
    S.selection = ids;
  }

  function render() {
    const v = view();
    if (v.view === 'refresh') refreshChoice();
    let parts;
    switch (v.view) {
      case 'unsupported':
        parts = renderUnsupported(v.reason);
        break;
      case 'none':
        parts = renderNone(v.reason);
        break;
      case 'disconnected':
        parts = renderDisconnected();
        break;
      case 'ready':
        parts = renderReady(v.reason);
        break;
      case 'blocked':
        parts = renderBlocked(v.reason);
        break;
      case 'refresh':
        parts = renderRefresh();
        break;
      case 'queued':
        parts = renderQueued();
        break;
      default:
        parts = renderLoading(v.reason);
    }
    if (S.message) parts.push(el('p', { class: `note${S.message.tone ? ' note-' + S.message.tone : ''}`, id: 'msg', role: 'status', text: S.message.text }));
    parts.push(overlay());
    root.dataset.view = v.view;
    root.dataset.reason = v.reason || '';
    root.replaceChildren(...parts.filter(Boolean));
    if (S.confirming) {
      const c = root.querySelector('#override-cancel');
      if (c) c.focus();
    }
    onView(v, S);
  }

  // ---------------------------------------------------------- actions

  function onTick(tr, box) {
    S.selection = toggleSelection(tracks(), S.selection, tr.id, box.checked);
    const same = PRESETS.find((p) => {
      const ids = presetSelection(tracks(), p, S.detection.presets || null);
      return ids && ids.length === S.selection.length && ids.every((x) => S.selection.includes(x));
    });
    S.preset = same || 'custom';
    S.picked = true;
    render();
  }

  function closeConfirm(confirmed) {
    S.confirming = false;
    if (confirmed) {
      S.override = true;
      send({ remember: S.rememberTick && !!platformName() });
      return;
    }
    render();
  }

  async function send({ remember = false } = {}) {
    if (S.busy) return;
    if (S.detection.downloadPolicy === 'disabled' && !S.override) {
      if (S.remembered) {
        // The user turned the reminder off for this platform: the override
        // goes with the send, no reminder.
        S.override = true;
      } else {
        // SPEC-ext: warn before overriding the platform's switch (once).
        S.confirming = true;
        S.rememberTick = false;
        render();
        return;
      }
    }
    S.busy = 'send';
    S.message = null;
    render();
    const r = await ask('send', { trackIds: S.selection, courseId: S.courseId, overrideDisabled: S.override, rememberPlatform: remember });
    S.busy = '';
    if (remember) S.remembered = true;
    if (r.ok) {
      S.task = { taskId: r.taskId, state: 'queued', position: r.position || 0 };
      S.status = { taskId: r.taskId, state: 'queued', position: r.position || 0, bytesDone: 0, bytesTotal: -1 };
      if (S.conn) S.conn.lastCourseId = S.courseId;
      render();
      pullStatus(r.taskId);
      return;
    }
    if (r.error && r.error.code === 'queue.duplicate') {
      const p = r.error.params || {};
      S.checked = true;
      S.checkItem = r.item || { sessionId: '', state: p.state, taskId: p.taskId, lectureId: p.lectureId || '' };
      if (S.checkItem.state !== 'imported' && S.checkItem.state !== 'needs_refresh') S.task = { taskId: S.checkItem.taskId, state: S.checkItem.state, position: 0 };
      render();
      return;
    }
    if (r.error && r.error.code === 'course.not_found') connect(true);
    say(errorText(r.error), 'bad');
  }

  async function refresh() {
    if (S.busy) return;
    S.busy = 'refresh';
    S.message = null;
    render();
    const r = await ask('refresh', { trackIds: S.selection });
    S.busy = '';
    if (r.ok) {
      S.task = { taskId: r.taskId, state: r.state || 'queued', position: 0 };
      S.status = { taskId: r.taskId, state: r.state || 'queued', position: 0, bytesDone: 0, bytesTotal: -1 };
      render();
      pullStatus(r.taskId);
      return;
    }
    say(errorText(r.error), 'bad');
  }

  async function openInApp(target) {
    const r = await ask('open', target);
    say(r.ok ? t('opened') : errorText(r.error), r.ok ? 'ok' : 'bad');
  }

  async function pullStatus(taskId) {
    const r = await ask('status', { taskId });
    if (r.ok && r.status) onStatus(r.status);
  }

  function onStatus(status) {
    if (!status) return;
    const taskId = (S.task && S.task.taskId) || (S.checkItem && S.checkItem.taskId);
    if (!taskId || status.taskId !== taskId) return;
    if (status.state === 'needs_refresh') {
      // The credentials ran out while the card is open: offer "续传".
      S.task = null;
      S.status = null;
      S.checkItem = { ...(S.checkItem || {}), taskId, state: 'needs_refresh', error: status.error || null };
      render();
      return;
    }
    S.status = status;
    if (!S.task) S.task = { taskId, state: status.state, position: status.position || 0 };
    else S.task.state = status.state;
    if (status.state === 'done' && status.lectureId && S.checkItem) S.checkItem.lectureId = status.lectureId;
    render();
  }

  // ---------------------------------------------------------- loading

  async function detect(force = false) {
    const r = await ask('detect', { force });
    if (!r.ok) S.detection = { status: 'no_access' };
    else {
      S.detection = r.view;
      S.origins = r.origins || [];
      S.cookieHosts = r.cookieHosts || [];
      S.offers = r.offers || [];
    }
    if (!r.ok && r.error) S.message = { text: errorText(r.error), tone: 'bad' };
    if (S.detection.status === 'ok') {
      S.selection = defaultSelection(tracks(), { downloadPolicy: S.detection.downloadPolicy, presets: S.detection.presets || null });
      S.preset = S.selection.length ? 'main' : '';
      S.picked = false;
      S.refreshChoice = false;
      S.override = false;
      S.remembered = !!S.detection.remembered;
    }
    S.checked = false;
    S.checkItem = null;
    render();
    await check();
  }

  /**
   * hello. The popup opening is no reason to start LectureFlow (T26 G1):
   * its first hello is passive (noLaunch) and only says whether LectureFlow
   * runs; "打开 LectureFlow" and "重新检测" may start it — but "重新检测"
   * beside "打开 LectureFlow" only looks again (passive). An in-page card or
   * the confirmation window was opened by the user's click on "发送至
   * LectureFlow", which may.
   */
  async function connect(recheck = false, { passive = host === 'popup' && !recheck } = {}) {
    // after the popup found LectureFlow installed but not open, a hello that
    // may start it does start it: "正在启动 LectureFlow…" (T26 G7)
    S.conn = { status: 'checking', launching: recheck && !passive && disconnectedWhy(S.conn) === 'not_open' };
    render();
    const r = await ask('connection', { recheck, passive });
    S.conn = r.ok ? r.connection : { status: 'unavailable', reason: r.error && r.error.code };
    if (S.conn.status === 'connected') {
      const ids = (S.conn.courses || []).map((c) => String(c.id));
      const last = S.conn.lastCourseId || '';
      S.courseId = ids.includes(S.courseId) ? S.courseId : ids.includes(last) ? last : ids[0] || '';
    }
    render();
    await check();
  }

  // The session's place in LectureFlow, once both detection and hello are in.
  let checking = null;
  async function check() {
    if (!S.detection || S.detection.status !== 'ok' || tracks().length === 0) return;
    if (!S.conn || S.conn.status !== 'connected' || S.checked || checking) return;
    checking = ask('check');
    const r = await checking;
    checking = null;
    S.checked = true;
    if (r.ok) {
      S.checkItem = r.item || null;
      if (r.item && r.status) S.status = r.status;
    } else S.checkItem = null; // could not check: the send path checks again
    render();
  }

  async function start() {
    render();
    await Promise.all([detect(true), connect(false)]);
  }

  return { start, onStatus, redetect: () => detect(true), state: S, render };
}
