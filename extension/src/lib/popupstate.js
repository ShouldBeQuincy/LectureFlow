// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which of the popup's views to show (SPEC-ext 界面设计: five states), from
// what the popup knows: the detection, the connection to LectureFlow, the
// check of this session, and what the user did in this popup. Pure.
//
//   loading       detection or hello still running; reason: '' (detecting),
//                 connecting | launching (hello), checking (this session)
//   unsupported   a do-not-handle site (src/lib/blocklist.js); reason: its
//                 category, youtube | courses
//   none          没探测到轨道
//   disconnected  LectureFlow 未连接
//   ready         可送入
//   blocked       不可送入: imported | disabled | protected
//   refresh       a needs_refresh task of this session: 续传
//   queued        已加入队列 (also a task of this session that is running)

import { checkOutcome, sendable } from './send.js';

/**
 * input: {
 *   detection: { status, tracks: [...], downloadPolicy } | null,
 *   connection: { status: 'checking' | 'connected' | 'unavailable', launching? } | null,
 *   checked: bool,          // the check answered (or could not run)
 *   checkItem: item | null, // this session's check item
 *   task: { taskId, state } | null, // sent or refreshed from this popup
 *   override: bool,         // the user overrode "下载已关闭"
 * }
 * Returns { view, reason }.
 */
export function popupView({ detection, connection, checked = false, checkItem = null, task = null, override = false }) {
  if (!detection || detection.status === 'loading') return { view: 'loading', reason: '' };
  if (detection.status === 'unsupported') return { view: 'unsupported', reason: detection.category || '' };
  if (detection.status !== 'ok') return { view: 'none', reason: detection.status };
  if (!detection.tracks || detection.tracks.length === 0) return { view: 'none', reason: 'no_tracks' };
  // The tracks are in; what the card still waits for (T26 G7): LectureFlow
  // answering hello, or starting (a hello that may start it, after the
  // popup found it not open), then the check of this session.
  if (!connection || connection.status === 'checking') return { view: 'loading', reason: connection && connection.launching ? 'launching' : 'connecting' };
  if (connection.status !== 'connected') return { view: 'disconnected', reason: connection.reason || '' };
  if (task) return { view: 'queued', reason: '' };
  if (!checked) return { view: 'loading', reason: 'checking' };
  const outcome = checkOutcome(checkItem);
  if (outcome === 'imported') return { view: 'blocked', reason: 'imported' };
  if (outcome === 'active') return { view: 'queued', reason: '' };
  if (outcome === 'needs_refresh') return { view: 'refresh', reason: '' };
  if (!detection.tracks.some(sendable)) return { view: 'blocked', reason: 'protected' };
  if (detection.downloadPolicy === 'disabled' && !override) return { view: 'blocked', reason: 'disabled' };
  return { view: 'ready', reason: outcome === 'again' ? checkItem.state : '' };
}

/**
 * The course and the send button stay in sight at the bottom of what
 * scrolls (T26 J6) only when at least SEND_ROOM px of it remain above them
 * (SEC42 中-1): a low in-page card would otherwise show little but them.
 * areaHeight: the visible height of what scrolls (the popup's or the
 * window's viewport, the in-page card's height); sendHeight: the block's.
 */
export const SEND_ROOM = 260;

export function sendSticks(areaHeight, sendHeight) {
  return Number.isFinite(areaHeight) && Number.isFinite(sendHeight) && sendHeight > 0 && areaHeight - sendHeight >= SEND_ROOM;
}

/** Progress of a status push: { percent (0–100 or -1), done, total }. */
export function progressOf(status) {
  const done = Number(status && status.bytesDone) || 0;
  const total = Number(status && status.bytesTotal);
  if (!(total > 0)) return { percent: -1, done, total: -1 };
  return { percent: Math.max(0, Math.min(100, Math.floor((done / total) * 100))), done, total };
}

/** Whether a task state is over (no more pushes expected). */
export function isFinal(state) {
  return state === 'done' || state === 'failed' || state === 'cancelled' || state === 'needs_refresh' || state === 'imported';
}
