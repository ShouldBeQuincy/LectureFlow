// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// Upload queue (SPEC §9.3, §9.4). Lives outside the views so an upload keeps
// going, and stays visible, while the user looks at another page.
// One upload at a time per course, in the order the files were added.

import { api, isLocalFile } from './api.js';
import { toast, choiceDialog } from './dom.js';
import * as U from './util.js';
import { t } from './i18n.js';

const queues = new Map(); // courseId -> {items: [], busy: bool}
const listeners = new Set();
let seq = 0;
let guard = null;

// setUploadGuard(fn): fn(file) returns an error text (UI language) when the upload is
// bound to be refused (over 4 GB, too little disk space). The server checks
// the same before reading the body, but then the browser often only sees a
// reset connection instead of the reason, so the reason is shown up front.
export function setUploadGuard(fn) {
  guard = fn;
}

// fn(courseId, item, result, progressOnly)
export function onUploadChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function emit(courseId, item, result = null, progressOnly = false) {
  for (const fn of listeners) fn(courseId, item, result, progressOnly);
}

export function uploadItems(courseId, kind) {
  const q = queues.get(courseId);
  return q ? q.items.filter((i) => !kind || i.kind === kind) : [];
}

// kind: lecture | material (the syllabus goes through the A8 editor, wizard.js)
export function enqueueUpload(courseId, kind, file, opts = {}) {
  let q = queues.get(courseId);
  if (!q) {
    q = { items: [], busy: false };
    queues.set(courseId, q);
  }
  const item = {
    key: 'up' + ++seq,
    courseId,
    kind,
    file,
    title: opts.title || '',
    lectureId: opts.lectureId || null,
    // desktop app: the server copies the file from its local path; no byte
    // progress, and the size is unknown (null) for a file picked in the
    // system dialog (SPEC-v3 V1.4)
    local: isLocalFile(file),
    status: 'waiting', // waiting | uploading | failed
    fraction: 0,
    loaded: 0,
    total: file.size,
    error: '',
    abort: null,
  };
  q.items.push(item);
  emit(courseId, item);
  pump(courseId);
  return item;
}

// Lecture files dropped on the course page or in the wizard's last step
// (SPEC §9.4: one file per lecture, title = file name without extension,
// drafts since v1.1 A3). Non-media files are skipped with a toast; files
// that differ only by extension ask which one to keep (§9.4.3).
// Resolves to the queued items.
export async function addLectureFiles(courseId, files) {
  const media = files.filter(U.isMediaFile);
  const skipped = files.filter((f) => !U.isMediaFile(f));
  if (skipped.length) toast(t('common.upload.skipped_media', { files: U.listJoin(skipped.map((f) => f.name)) }), 'warn');
  let keep = media;
  for (const group of U.sameStemGroups(media)) {
    const choice = await choiceDialog({
      title: t('common.upload.same_title'),
      message: group.length === 2 ? t('common.upload.same_two') : t('common.upload.same_many'),
      choices: group.map((f) => ({ value: f, label: f.name, hint: f.size == null ? '' : U.fmtBytes(f.size) })),
      okText: t('common.upload.same_ok'),
      cancelText: t('common.upload.same_none'),
      // R62 N1: the queue is global and its cancel means "none": the
      // question stays when the page changes (not closed as cancelled)
      route: false,
    });
    keep = keep.filter((f) => !group.includes(f) || f === choice);
  }
  // The queue is global, so this still works if the user left the page meanwhile.
  return keep.map((f) => enqueueUpload(courseId, 'lecture', f, { title: U.stripExt(f.name) }));
}

// Slide PDFs (SPEC §9.3); lectureId null = for the whole course. Resolves to the queued items.
export function addMaterialFiles(courseId, files, lectureId = null) {
  const pdfs = files.filter(U.isPDF);
  const skipped = files.filter((f) => !U.isPDF(f));
  if (skipped.length) toast(t('common.upload.skipped_pdf', { files: U.listJoin(skipped.map((f) => f.name)) }), 'warn');
  return pdfs.map((f) => enqueueUpload(courseId, 'material', f, { lectureId }));
}

export function retryUpload(item) {
  if (item.status !== 'failed') return;
  item.status = 'waiting';
  item.error = '';
  emit(item.courseId, item);
  pump(item.courseId);
}

// Removes a waiting/failed item, or aborts a running one.
export function removeUpload(item) {
  const q = queues.get(item.courseId);
  if (!q) return;
  if (item.status === 'uploading') {
    if (item.abort) item.abort();
    return; // pump's catch removes it
  }
  const i = q.items.indexOf(item);
  if (i >= 0) q.items.splice(i, 1);
  emit(item.courseId, item);
}

function start(item, onProgress) {
  if (item.kind === 'lecture') return api.uploadLecture(item.courseId, item.title, item.file, onProgress);
  return api.uploadMaterial(item.courseId, item.file, item.lectureId, onProgress);
}

async function pump(courseId) {
  const q = queues.get(courseId);
  if (!q || q.busy) return;
  const item = q.items.find((i) => i.status === 'waiting');
  if (!item) return;
  const refused = guard ? guard(item.file) : null;
  if (refused) {
    item.status = 'failed';
    item.error = refused;
    emit(courseId, item);
    toast(t('common.upload.refused', { name: item.file.name, reason: refused }), 'warn');
    pump(courseId);
    return;
  }
  q.busy = true;
  item.status = 'uploading';
  item.fraction = 0;
  item.loaded = 0;
  emit(courseId, item);
  try {
    const { promise, abort } = start(item, (fraction, loaded, total) => {
      item.fraction = fraction == null ? null : fraction;
      item.loaded = loaded;
      item.total = total;
      emit(courseId, item, null, true);
    });
    item.abort = abort;
    const result = await promise;
    q.items.splice(q.items.indexOf(item), 1);
    toast(item.local ? t('common.native.copy_done', { name: item.file.name }) : t('common.upload.done', { name: item.file.name }), 'success');
    emit(courseId, item, result);
  } catch (err) {
    if (err && err.status === -1) {
      const i = q.items.indexOf(item);
      if (i >= 0) q.items.splice(i, 1);
      toast(item.local ? t('common.native.copy_cancelled', { name: item.file.name }) : t('common.upload.cancelled', { name: item.file.name }), 'info');
    } else {
      item.status = 'failed';
      item.error = (err && err.message) || t('common.upload.failed_plain');
    }
    emit(courseId, item);
  } finally {
    item.abort = null;
    q.busy = false;
    pump(courseId);
  }
}
