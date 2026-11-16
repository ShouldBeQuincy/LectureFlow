// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// v1.1 A8: the add-course wizard and the SYLLABUS.md editor.
//
// openWizard(ctx) — a modal in five steps (A8.1): basics → syllabus →
// format preview → slides → done. Closing with unsaved work asks first; Esc
// closes (with that prompt), focus stays inside the dialog, Ctrl+Enter (⌘+Enter
// on a Mac, where the hints say ⌘; 3.1.0 M8) is the
// primary action.
// The step rail goes back to any earlier step, and forward to a step seen
// before as long as every step in between is still complete (saved);
// otherwise the step says why it cannot be opened. The course is created
// when leaving step 1 (later changes to the name or code are PATCHed; the
// v2 P13.4 term start and skipped weeks are PATCHed right after creation), so
// the syllabus, slides and lectures of the following steps are attached to
// it right away. Files dropped anywhere on the dialog go to the current step.
//
// openSyllabusEditor(course, opts) — the course page's "Edit syllabus" (A8.2):
// the same source picker and format/preview editor as steps 2–3. Saving
// re-runs glossary_seed on the server (manual terms are never overwritten).
//
// Background work (term extraction, slides) is followed through the app's
// SSE stream (ctx.subscribe), never polled. Requests of a step or dialog
// that is left are aborted (an aborted format request stops the server's
// LLM call). Nothing is changed optimistically: every state shown comes
// from an API response; errors show the server's text (toasted by the API
// layer, or inside the dialog where they have a place of their own).

import { api, isLocalFile } from './api.js';
import { h, clear, toast, confirmDialog, btn, iconBtn, makeDropTarget, dropzone, filePicker, errorBlock, skeleton, preserveFocus, focusOpener } from './dom.js';
import { icon } from './icons.js';
import { renderMarkdown } from './markdown.js';
import { addLectureFiles, addMaterialFiles, onUploadChange, uploadItems, retryUpload, removeUpload } from './uploads.js';
import { uploadRow, updateUploadRow } from './widgets.js';
import * as U from './util.js';
import { t } from './i18n.js';
import { calendarFields, courseNameError as nameError, courseCodeError as codeError, COURSE_NAME_MAX as NAME_MAX, COURSE_CODE_MAX as CODE_MAX } from './calendar_ui.js';
import { kindTag } from './kinds.js';
import { bindSelect, weekChoices } from './material_bind.js';

const SYLLABUS_ACCEPT = '.txt,.md,.pdf,text/plain,text/markdown,application/pdf';
const MEDIA_ACCEPT = U.MEDIA_ACCEPT; // extensions only (util.js, security review 2 M3)
// NAME_MAX, CODE_MAX, nameError, codeError: calendar_ui.js (the server's
// limits, shared with the course settings, T22 B1)
const REFRESH_MS = 300; // SSE events → re-read the course (debounced)

let uid = 0;

// ---------- small helpers ----------

// Course part of a citation ref when the course has no code (A11.3): the
// first character of every run of letters/digits, upper-cased.
function courseAbbr(name) {
  const words = (name || '').match(/[\p{L}\p{N}][\p{L}\p{N}']*/gu) || [];
  return words.map((w) => w[0].toUpperCase()).join('') || 'COURSE';
}

function chars(s) {
  return U.fmtInt((s || '').length);
}

// "12,345 characters" (n selects the plural form, count is the formatted number).
function charsText(s) {
  return t('wizard.syllabus.chars', { n: (s || '').length, count: chars(s) });
}

// A busy primary button shows a spinning loader instead of its icon.
function spinIfBusy(b, busy) {
  if (busy) b.querySelector('svg')?.classList.add('spin');
  return b;
}

function isPrimaryKey(e) {
  return e.key === 'Enter' && (e.ctrlKey || e.metaKey) && !e.isComposing;
}

const isAbort = (e) => !!e && e.name === 'AbortError';

// A native modal <dialog>: focus stays inside it and the page behind is
// inert. Every way of closing (Esc, the backdrop, the × button) goes through
// onRequestClose, which decides whether to ask first. If the browser closes
// the dialog by itself (Chrome does so on a repeated Esc), it is reopened
// and the request goes through the same path.
function modal(className, label, onRequestClose) {
  // The modal stays across a route change; closed on another page, the
  // focus goes back as after a dialog (T21 M4, dom.js focusOpener).
  const back = focusOpener();
  const dlg = h('dialog', { class: `dlg ${className}`, 'aria-label': label });
  const toasts = document.getElementById('toasts');
  let closing = false;
  let asking = false; // one close request at a time (one prompt)
  const request = async () => {
    if (asking) return;
    asking = true;
    try {
      await onRequestClose();
    } finally {
      asking = false;
    }
  };
  dlg.addEventListener('cancel', (e) => {
    // Without recent user activation Chrome's cancel is not cancelable and
    // the dialog closes regardless: that case is handled on 'close' below,
    // after the dialog is back, so the prompt opens on top of it.
    if (!e.cancelable) return;
    e.preventDefault();
    request();
  });
  dlg.addEventListener('close', () => {
    if (closing || !dlg.isConnected) return;
    dlg.showModal();
    if (toasts) dlg.append(toasts);
    request();
  });
  dlg.addEventListener('mousedown', (e) => {
    if (e.target !== dlg) return;
    const r = dlg.getBoundingClientRect();
    if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) onRequestClose();
  });
  // Toasts live outside the dialog and would sit behind its backdrop
  // (blurred, inert); while the dialog is open it hosts them, so the
  // server's error texts stay readable (SPEC §9.8).
  return {
    dlg,
    show() {
      document.body.append(dlg);
      dlg.showModal();
      if (toasts) dlg.append(toasts);
    },
    close() {
      closing = true;
      if (toasts && toasts.parentNode === dlg) document.body.append(toasts);
      if (dlg.open) dlg.close();
      dlg.remove();
      back(document.getElementById('main'));
    },
  };
}

function lostWorkDialog(reasons, extra) {
  return confirmDialog({
    title: t('wizard.close.title'),
    message: reasons.map((r) => `· ${r}`).join('\n') + (extra ? `\n\n${extra}` : ''),
    okText: t('wizard.close.discard'),
    cancelText: t('wizard.close.keep'),
    danger: true,
  });
}

// ---------- the syllabus flow (wizard steps 2–3, course-page editor) ----------

// syllabusFlow(courseId: () => string, {allowSkip, autoFormat, onChange})
// keeps the state of picking the raw syllabus (paste / upload / skip),
// formatting it with the syllabus_format role (A8.3) and editing the
// result. Views are built by sourceView() and formatView() and updated in
// place, so typing and uploads never lose the caret or focus.
// autoFormat: showing the format view formats a raw text that changed (the
// wizard's step 3); otherwise only an explicit action calls the model.
function syllabusFlow(courseId, opts) {
  const S = {
    mode: null, // paste | upload | skip
    text: '',
    sentText: null, // the pasted text last saved as raw
    upload: null, // {file, status: uploading|done|failed, fraction, loaded, total, error, errStatus, abort, promise}
    rawFrom: null, // 'paste' | the upload object | 'server': what syllabus.raw.txt holds now
    doc: { filename: '', raw: '', markdown: '' },
    rawRev: 0, // bumps whenever the raw text on the server changes
    // fmt.rev: the raw revision the SYLLABUS.md shown belongs to (formatted
    // from it, loaded or saved with it, or "use the raw text"); runRev: the
    // revision a running request formats; base: the model's result as it came.
    fmt: { status: 'idle', rev: 0, runRev: 0, base: '', usage: null, error: '', t0: 0, seq: 0, ctl: null },
    md: '',
    savedMd: null,
    view: 'preview', // preview | edit
    rawHidden: false,
    formatShown: false,
  };
  const R = {}; // DOM refs of the views currently built
  const life = new AbortController(); // aborted by destroy(): requests of a closed dialog
  let tick = null;
  let alive = true;
  const changed = () => opts.onChange && opts.onChange();

  // ---- source (step 2) ----

  function sourceView() {
    const name = 'sylsrc' + ++uid;
    const modes = [
      ['paste', 'pencil', t('wizard.syllabus.paste'), t('wizard.syllabus.paste_hint')],
      ['upload', 'upload', t('wizard.syllabus.upload'), t('wizard.syllabus.upload_hint')],
      opts.allowSkip ? ['skip', 'chevron-right', t('wizard.syllabus.skip'), t('wizard.syllabus.skip_hint')] : null,
    ].filter(Boolean);
    R.radios = new Map();
    const cards = h(
      'div',
      { class: 'wiz-choices' + (modes.length === 2 ? ' wiz-choices-2' : ''), role: 'radiogroup', 'aria-label': t('wizard.syllabus.source_aria') },
      modes.map(([v, ic, label, hint]) => {
        const input = h('input', {
          type: 'radio',
          name,
          value: v,
          checked: S.mode === v,
          onchange: () => {
            S.mode = v;
            // a click moves on into the panel; arrow keys stay in the radio group
            renderSourcePanel(performance.now() - (R.pointerAt || 0) < 600);
            changed();
          },
        });
        R.radios.set(v, input);
        return h(
          'label',
          { class: 'wiz-choice', onpointerdown: () => (R.pointerAt = performance.now()) },
          input,
          h('span', { class: 'wiz-choice-icon' }, icon(ic, { size: 18 })),
          h('span', { class: 'wiz-choice-text' }, h('span', { class: 'wiz-choice-label' }, label), h('span', { class: 'wiz-choice-hint' }, hint)),
        );
      }),
    );
    R.panel = h('div', { class: 'syl-src-panel' });
    R.textarea = null;
    const root = h('div', { class: 'syl-src' }, cards, R.panel);
    makeDropTarget(root, pickFile);
    renderSourcePanel(false);
    return root;
  }

  function setMode(v) {
    S.mode = v;
    const r = R.radios && R.radios.get(v);
    if (r) r.checked = true;
  }

  function renderSourcePanel(focus) {
    if (!R.panel) return;
    clear(R.panel);
    R.panel.dataset.mode = S.mode || '';
    if (S.mode === 'paste') {
      if (!R.textarea) {
        R.count = h('span', { class: 'syl-count' });
        R.textarea = h('textarea', {
          class: 'input syl-paste',
          rows: '12',
          spellcheck: 'false',
          placeholder: t('wizard.syllabus.paste_placeholder'),
          'aria-label': t('wizard.syllabus.paste_aria'),
          value: S.text,
        });
        R.textarea.addEventListener('input', () => {
          S.text = R.textarea.value;
          syncCount();
          changed();
        });
      }
      syncCount();
      R.panel.append(R.textarea, h('div', { class: 'syl-paste-foot' }, R.count, h('span', { class: 'muted small' }, t('wizard.syllabus.paste_note'))));
      if (focus) R.textarea.focus();
    } else if (S.mode === 'upload') {
      R.panel.append(uploadBox());
      if (focus) {
        const b = R.panel.querySelector('button');
        if (b) b.focus();
      }
    } else if (S.mode === 'skip') {
      R.panel.append(
        h(
          'div',
          { class: 'syl-note' },
          icon('circle-alert', { size: 16 }),
          h('div', null, h('strong', null, t('wizard.syllabus.skip_title')), h('p', null, t('wizard.syllabus.skip_body'))),
        ),
      );
    } else {
      R.panel.append(h('p', { class: 'syl-src-empty' }, t('wizard.syllabus.choose')));
    }
  }

  function syncCount() {
    if (R.count) R.count.textContent = S.text ? charsText(S.text) : '';
  }

  function uploadBox() {
    const up = S.upload;
    if (!up) {
      return dropzone({ text: t('wizard.syllabus.drop'), accept: SYLLABUS_ACCEPT, multiple: false, onFiles: pickFile, target: false });
    }
    R.upStatus = h('span', { class: 'syl-file-status' });
    R.upProgress = up.status === 'uploading' ? h('progress', { class: 'up-progress', max: '1', 'aria-label': up.file.name }) : null; // named by its file (L16)
    const actions = [];
    if (up.status === 'uploading') actions.push(btn(t('wizard.upload.cancel'), { size: 'sm', onclick: () => up.abort && up.abort() }));
    // Retrying only helps when the connection or the server failed; a file
    // the server refused (400 type, 413 size, 422 no text layer) needs another file.
    const s = up.errStatus;
    const retryable = up.status === 'failed' && (!s || s >= 500 || s === 408 || s === 429);
    if (retryable) actions.push(btn(t('common.retry'), { size: 'sm', kind: 'primary', icon: 'rotate-cw', onclick: () => startUpload(up.file) }));
    if (up.status !== 'uploading') {
      const picker = filePickerFor();
      actions.push(btn(t('wizard.syllabus.replace_file'), { size: 'sm', kind: up.status === 'failed' && !retryable ? 'primary' : undefined, onclick: picker.open }), picker.input);
    }
    const box = h(
      'div',
      { class: `syl-file is-${up.status}` },
      h('span', { class: 'syl-file-icon' }, icon(up.status === 'failed' ? 'circle-alert' : up.status === 'done' ? 'circle-check' : 'file-text', { size: 18 })),
      h('div', { class: 'syl-file-main' }, h('span', { class: 'fname', text: up.file.name, title: up.file.name }), R.upStatus, R.upProgress),
      h('div', { class: 'syl-file-actions' }, actions),
    );
    syncUpload();
    return box;
  }

  // the system dialog in the desktop app (dom.filePicker, SPEC-v3 V1.4)
  function filePickerFor() {
    return filePicker({ accept: SYLLABUS_ACCEPT, multiple: false, onFiles: pickFile });
  }

  function syncUpload() {
    const up = S.upload;
    if (!up || !R.upStatus) return;
    const size = U.fmtBytes(up.file.size);
    if (isLocalFile(up.file)) {
      // desktop app: copied and read by the server, no byte progress (SPEC-v3 V1.4)
      if (up.status === 'uploading') {
        R.upStatus.textContent = t('common.native.syllabus_working');
        if (R.upProgress) R.upProgress.removeAttribute('value');
      } else if (up.status === 'done') {
        R.upStatus.textContent = t('common.native.syllabus_copied', { n: (S.doc.raw || '').length, count: chars(S.doc.raw) });
      } else {
        R.upStatus.textContent = t('wizard.upload.failed', { error: up.error });
      }
      return;
    }
    if (up.status === 'uploading') {
      R.upStatus.textContent =
        up.fraction == null ? t('wizard.upload.uploading', { size }) : up.fraction >= 1 ? t('wizard.syllabus.reading', { size }) : t('wizard.upload.progress', { size, pct: U.pct(up.fraction) });
      if (R.upProgress) {
        if (up.fraction == null || up.fraction >= 1) R.upProgress.removeAttribute('value');
        else R.upProgress.value = up.fraction;
      }
    } else if (up.status === 'done') {
      R.upStatus.textContent = t('wizard.syllabus.uploaded', { size, n: (S.doc.raw || '').length, count: chars(S.doc.raw) });
    } else {
      R.upStatus.textContent = t('wizard.upload.failed', { error: up.error });
    }
  }

  function pickFile(files) {
    if (files.length > 1) {
      toast(t('wizard.syllabus.one_file'), 'warn');
      return;
    }
    const f = files[0];
    if (!U.isSyllabusFile(f)) {
      toast(t('errors.syllabus.bad_type', { filename: f.name }), 'warn');
      return;
    }
    setMode('upload');
    startUpload(f);
    changed();
  }

  function startUpload(file) {
    if (S.upload && S.upload.status === 'uploading' && S.upload.abort) S.upload.abort();
    const up = { file, status: 'uploading', fraction: 0, loaded: 0, total: file.size, error: '', errStatus: 0, abort: null, promise: null };
    S.upload = up;
    // quiet: the upload box shows the server's error inline
    const { promise, abort } = api.uploadSyllabusRaw(
      courseId(),
      file,
      (fraction, loaded, total) => {
        up.fraction = fraction;
        up.loaded = loaded;
        up.total = total;
        if (S.upload === up) syncUpload();
      },
      { quiet: true },
    );
    up.abort = abort;
    up.promise = promise.then(
      (doc) => {
        if (!alive || S.upload !== up) return false;
        up.status = 'done';
        S.doc = doc || S.doc;
        S.rawFrom = up;
        S.rawRev++;
        renderSourcePanel(false);
        changed();
        return true;
      },
      (err) => {
        if (!alive || S.upload !== up) return false;
        if (err && err.status === -1) S.upload = null;
        else {
          up.status = 'failed';
          up.error = (err && err.message) || t('wizard.upload.failed_short');
          up.errStatus = (err && err.status) || 0;
        }
        renderSourcePanel(false);
        changed();
        return false;
      },
    );
    if (S.mode === 'upload') renderSourcePanel(false);
  }

  // commitSource() saves what step 2 picked as syllabus.raw.txt. Resolves to
  // 'format' (go on to formatting), 'skip', or null (stay; reason toasted).
  async function commitSource() {
    if (S.mode === 'skip') return 'skip';
    if (S.mode === 'paste') {
      if (!S.text.trim()) {
        toast(t('wizard.syllabus.need_paste'), 'warn');
        if (R.textarea) R.textarea.focus();
        return null;
      }
      if (S.rawFrom !== 'paste' || S.text !== S.sentText) {
        const text = S.text;
        const doc = await api.syllabusRawText(courseId(), text, { signal: life.signal }); // errors are toasted; the caller stays
        if (!alive) return null;
        S.doc = doc || S.doc;
        S.sentText = text;
        S.rawFrom = 'paste';
        S.rawRev++;
      }
      return 'format';
    }
    if (S.mode === 'upload') {
      const up = S.upload;
      if (!up) {
        toast(t('wizard.syllabus.need_file'), 'warn');
        return null;
      }
      if (up.status === 'uploading') {
        toast(t('wizard.syllabus.still_uploading'), 'info');
        return null;
      }
      if (up.status === 'failed') {
        toast(t('wizard.syllabus.upload_failed_retry'), 'warn');
        return null;
      }
      if (S.rawFrom !== up) {
        // the pasted text was saved after this file: send the file again
        startUpload(up.file);
        return (await S.upload.promise) ? 'format' : null;
      }
      return 'format';
    }
    toast(opts.allowSkip ? t('wizard.syllabus.choose_mode_skip') : t('wizard.syllabus.choose_mode'), 'warn');
    return null;
  }

  // Why what step 2 shows is not what the server holds yet ('' when it is,
  // or when the syllabus is skipped).
  function sourceIncomplete() {
    if (S.mode === 'skip') return '';
    if (S.mode === 'paste') {
      if (!S.text.trim()) return t('wizard.syllabus.why.no_paste');
      if (S.rawFrom !== 'paste' || S.text !== S.sentText) return t('wizard.syllabus.why.paste_unsent');
      return '';
    }
    if (S.mode === 'upload') {
      const up = S.upload;
      if (!up) return t('wizard.syllabus.why.no_file');
      if (up.status === 'uploading') return t('wizard.syllabus.why.uploading');
      if (up.status === 'failed') return t('wizard.syllabus.why.upload_failed');
      if (S.rawFrom !== up) return t('wizard.syllabus.why.file_unsent');
      return '';
    }
    return t('wizard.syllabus.why.no_source');
  }

  // ---- format and edit (step 3) ----

  function formatView() {
    S.formatShown = true;
    R.rawMeta = h('span', { class: 'syl-pane-meta' });
    R.rawText = h('pre', { class: 'syl-raw-text', tabindex: '0', 'aria-label': t('wizard.format.raw_aria') });
    R.rawHide = btn(t('wizard.format.hide_raw'), { size: 'sm', kind: 'ghost', onclick: toggleRaw, title: t('wizard.format.hide_raw_tip') });
    R.rawShow = btn(t('wizard.format.show_raw'), { size: 'sm', kind: 'ghost', icon: 'file-text', onclick: toggleRaw });
    R.viewBtns = {
      preview: h('button', { type: 'button', class: 'syl-seg-btn', onclick: () => setView('preview') }, icon('book-open', { size: 14 }), t('wizard.format.preview')),
      edit: h('button', { type: 'button', class: 'syl-seg-btn', onclick: () => setView('edit') }, icon('pencil', { size: 14 }), t('common.edit')),
    };
    R.regen = btn(t('wizard.format.regenerate'), { size: 'sm', icon: 'refresh-cw', onclick: regenerate, title: t('wizard.format.regenerate_tip') });
    R.dirtyChip = h('span', { class: 'chip chip-warn syl-dirty', hidden: true }, t('wizard.format.unsaved'));
    R.notice = h('div', { class: 'syl-notice', role: 'status', hidden: true });
    R.preview = h('div', { class: 'md syl-preview', tabindex: '0', 'aria-label': t('wizard.format.preview_aria') });
    R.previewMd = null; // the SYLLABUS.md the preview shows (renderFormat)
    R.editor = h('textarea', { class: 'syl-editor', spellcheck: 'false', 'aria-label': t('wizard.format.editor_aria'), value: S.md });
    R.editorFresh = true; // assigning .value puts the caret at the end
    R.editor.addEventListener('input', () => {
      S.md = R.editor.value;
      syncDirty();
      changed();
    });
    R.state = h('div', { class: 'syl-state' });
    R.usage = h('footer', { class: 'syl-pane-ft' });
    R.split = h(
      'div',
      { class: 'syl-split' },
      h(
        'section',
        { class: 'syl-pane syl-pane-raw', 'aria-label': t('wizard.format.raw') },
        h('header', { class: 'syl-pane-hd' }, h('span', { class: 'syl-pane-title' }, icon('file-text', { size: 14 }), t('wizard.format.raw')), R.rawMeta, R.rawHide),
        R.rawText,
      ),
      h(
        'section',
        { class: 'syl-pane syl-pane-md', 'aria-label': 'SYLLABUS.md' },
        h(
          'header',
          { class: 'syl-pane-hd' },
          h('span', { class: 'syl-pane-title' }, icon('sparkles', { size: 14 }), 'SYLLABUS.md'),
          R.dirtyChip,
          h('span', { class: 'syl-pane-tools' }, R.rawShow, h('span', { class: 'syl-seg', role: 'group', 'aria-label': t('wizard.format.view_aria') }, R.viewBtns.preview, R.viewBtns.edit), R.regen),
        ),
        R.notice,
        h('div', { class: 'syl-md-body' }, R.state, R.preview, R.editor),
        R.usage,
      ),
    );
    R.split.addEventListener('keydown', (e) => {
      // Ctrl+E (⌘E on a Mac) switches between preview and edit (the textarea keeps its caret)
      if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'e' && S.md && S.fmt.status !== 'running') {
        e.preventDefault();
        setView(S.view === 'edit' ? 'preview' : 'edit');
      }
    });
    renderRaw();
    renderFormat();
    return R.split;
  }

  function renderRaw() {
    if (!R.rawText) return;
    R.rawText.textContent = S.doc.raw || '';
    R.rawMeta.textContent = [S.doc.filename && S.doc.filename !== 'pasted.txt' ? S.doc.filename : S.doc.raw ? t('wizard.format.pasted') : '', S.doc.raw ? charsText(S.doc.raw) : ''].filter(Boolean).join(' · ');
    R.split.classList.toggle('raw-hidden', S.rawHidden);
    R.rawShow.hidden = !S.rawHidden;
  }

  function toggleRaw() {
    S.rawHidden = !S.rawHidden;
    renderRaw();
    (S.rawHidden ? R.rawShow : R.rawHide).focus({ preventScroll: true });
  }

  function setView(v) {
    S.view = v;
    renderFormat();
    if (v === 'edit') {
      if (R.editorFresh) {
        R.editor.setSelectionRange(0, 0);
        R.editor.scrollTop = 0;
        R.editorFresh = false;
      }
      R.editor.focus({ preventScroll: true });
    } else R.preview.focus({ preventScroll: true });
  }

  function syncDirty() {
    if (R.dirtyChip) R.dirtyChip.hidden = !(S.md.trim() && S.md !== S.savedMd);
  }

  function renderFormat() {
    if (!R.state) return;
    renderRaw();
    const f = S.fmt;
    const running = f.status === 'running';
    const hasMd = !!S.md.trim();
    clear(R.state);
    R.state.hidden = true;
    let showDoc = hasMd && !running;
    if (running) {
      R.state.hidden = false;
      R.elapsed = h('span', { class: 'syl-elapsed' });
      R.state.append(
        h(
          'div',
          { class: 'syl-busy', role: 'status' },
          h('div', { class: 'syl-busy-head' }, icon('loader-circle', { size: 18, class: 'spin' }), h('strong', null, t('wizard.format.running')), R.elapsed),
          h('p', null, t('wizard.format.running_body')),
          h(
            'div',
            { class: 'syl-busy-skel', 'aria-hidden': 'true' },
            h('div', { class: 'skel skel-h1' }),
            skeleton(2),
            h('div', { class: 'skel skel-title' }),
            skeleton(4),
            h('div', { class: 'skel skel-title' }),
            skeleton(3),
          ),
        ),
      );
      syncElapsed();
    } else if (f.status === 'error') {
      R.state.hidden = false;
      R.state.append(formatError(hasMd));
    } else if (!hasMd) {
      R.state.hidden = false;
      R.state.append(
        h(
          'div',
          { class: 'syl-empty' },
          h('div', { class: 'syl-empty-icon' }, icon('sparkles', { size: 22 })),
          h('strong', null, S.doc.raw ? t('wizard.format.empty_title') : t('wizard.format.no_raw_title')),
          h('p', null, S.doc.raw ? t('wizard.format.empty_body') : t('wizard.format.no_raw_body')),
          S.doc.raw
            ? h(
                'div',
                { class: 'syl-empty-actions' },
                btn(t('wizard.format.run'), { kind: 'primary', icon: 'sparkles', onclick: () => runFormat() }),
                btn(t('wizard.format.use_raw'), { onclick: useRaw }),
              )
            : null,
        ),
      );
    }
    if (f.status === 'error' && hasMd) showDoc = true;
    // The raw text changed after this SYLLABUS.md was made, and it was not
    // re-formatted by itself (edits would be lost, or the editor only calls
    // the model when asked): say so and offer it.
    const outdated = showDoc && f.status !== 'error' && S.rawRev > 0 && f.rev !== S.rawRev;
    R.notice.hidden = !outdated;
    clear(R.notice);
    if (outdated) {
      R.notice.append(
        icon('triangle-alert', { size: 15 }),
        h('span', null, editsAtRisk() ? t('wizard.format.outdated_edited') : t('wizard.format.outdated')),
        btn(t('wizard.format.reformat'), { size: 'sm', icon: 'refresh-cw', onclick: regenerate }),
      );
    }
    const edit = S.view === 'edit';
    R.preview.hidden = !showDoc || edit;
    R.editor.hidden = !showDoc || !edit;
    R.split.classList.toggle('is-busy', running);
    // R80 low-3: drawn again only when what it shows changed. A source over
    // 256 KiB is rendered in slices (markdown.js): drawn anew, the place
    // scrolled to in .syl-md-body was held to the first slice's height and
    // did not come back ("preview" pressed again, a save). What changes
    // S.md hides the preview first (a format running, the editor), which
    // loses the place in any case, as it did before the slices.
    if (showDoc && !edit && R.previewMd !== S.md) {
      clear(R.preview);
      R.preview.append(renderMarkdown(S.md));
      R.previewMd = S.md;
    }
    for (const [k, b] of Object.entries(R.viewBtns)) {
      b.setAttribute('aria-pressed', String(S.view === k));
      b.disabled = !showDoc;
    }
    R.regen.disabled = running || !S.doc.raw;
    syncDirty();
    clear(R.usage);
    if (f.usage && U.hasUsage(f.usage)) {
      R.usage.append(icon('gauge', { size: 13 }), h('span', null, t('wizard.format.usage', { usage: U.usageText(f.usage) })));
    } else {
      R.usage.append(h('span', null, U.macKeys(edit ? t('wizard.format.edit_hint') : t('wizard.format.preview_hint'))));
    }
  }

  // A failed format request, in the SYLLABUS.md pane: the server's text
  // (502 model error, 409 role not configured, …), retry, and — when there
  // is nothing to show yet — the raw text as SYLLABUS.md, so a missing
  // model never blocks the course.
  function formatError(hasMd) {
    const f = S.fmt;
    const msg = f.error.startsWith(t('errors.syllabus.format_failed')) ? f.error : t('wizard.format.failed_msg', { error: f.error });
    const useRawToo = !hasMd && !!S.doc.raw;
    return h(
      'div',
      { class: 'syl-err' + (hasMd ? ' is-inline' : ''), role: 'alert' },
      h(
        'div',
        { class: 'syl-err-box' },
        icon('circle-alert', { size: 18 }),
        h(
          'div',
          { class: 'syl-err-text' },
          h('strong', null, t('wizard.format.failed')),
          h('span', null, msg),
          hasMd ? h('span', { class: 'syl-err-hint' }, t('wizard.format.failed_keep')) : useRawToo ? h('span', { class: 'syl-err-hint' }, t('wizard.format.failed_use_raw')) : null,
        ),
        h('div', { class: 'syl-err-actions' }, btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', onclick: () => runFormat() }), useRawToo ? btn(t('wizard.format.use_raw'), { size: 'sm', onclick: useRaw }) : null),
      ),
    );
  }

  function useRaw() {
    const f = S.fmt;
    S.md = S.doc.raw;
    f.base = S.md;
    f.rev = S.rawRev;
    f.status = 'idle';
    f.error = '';
    f.usage = null;
    R.editor.value = S.md;
    R.editorFresh = true;
    syncDirty();
    changed();
    setView('edit');
  }

  function syncElapsed() {
    if (R.elapsed && S.fmt.status === 'running') R.elapsed.textContent = t('wizard.format.elapsed', { n: Math.max(0, Math.round((Date.now() - S.fmt.t0) / 1000)) });
  }

  function startTick() {
    stopTick();
    tick = setInterval(syncElapsed, 1000);
  }

  function stopTick() {
    clearInterval(tick);
    tick = null;
  }

  // Unsaved changes the user made to SYLLABUS.md (not the model's result as it came).
  function editsAtRisk() {
    return !!S.md.trim() && S.md !== S.savedMd && S.md !== S.fmt.base;
  }

  // Called when the format view is shown: with autoFormat, formats a raw
  // text that changed since the SYLLABUS.md shown (or restarts a run that
  // is formatting an older one) — unless that would replace the user's
  // unsaved edits; the pane then offers "re-format" instead.
  function ensureFormatted() {
    const f = S.fmt;
    if (S.rawRev === 0) return;
    if (f.status === 'running') {
      if (f.runRev !== S.rawRev) runFormat();
      return;
    }
    if (f.rev === S.rawRev || !opts.autoFormat) return;
    if (!editsAtRisk()) runFormat();
  }

  async function regenerate() {
    if (S.md.trim() && S.md !== S.fmt.base && S.md !== S.savedMd) {
      const ok = await confirmDialog({ title: t('wizard.format.regenerate'), message: t('wizard.format.regenerate_confirm'), okText: t('wizard.format.regenerate') });
      if (!ok || !alive) return;
    }
    runFormat();
  }

  async function runFormat() {
    const f = S.fmt;
    if (f.ctl) f.ctl.abort();
    const ctl = new AbortController();
    f.ctl = ctl;
    const seq = ++f.seq;
    f.status = 'running';
    f.error = '';
    f.t0 = Date.now();
    f.runRev = S.rawRev;
    startTick();
    renderFormat();
    changed();
    try {
      // quiet: a failure is shown in the SYLLABUS.md pane, with retry
      const res = await api.formatSyllabus(courseId(), { quiet: true, signal: ctl.signal });
      if (!alive || seq !== f.seq) return;
      f.status = 'done';
      f.rev = f.runRev;
      f.base = (res && res.markdown) || '';
      f.usage = (res && res.usage) || null;
      S.md = f.base;
      S.view = 'preview';
      if (R.editor) {
        R.editor.value = S.md;
        R.editorFresh = true;
      }
    } catch (e) {
      if (!alive || seq !== f.seq || isAbort(e)) return; // closed, superseded or cancelled
      f.status = 'error';
      f.error = (e && e.message) || t('common.unknown_error');
    } finally {
      if (f.ctl === ctl) f.ctl = null;
    }
    stopTick();
    renderFormat();
    changed();
  }

  // Stops a running format request (leaving step 3): aborting it makes the
  // server stop the LLM call. The previous SYLLABUS.md, if any, stays; the
  // wizard formats again when step 3 is shown next.
  function cancelFormat() {
    const f = S.fmt;
    if (f.status !== 'running') return;
    f.seq++;
    if (f.ctl) f.ctl.abort();
    f.ctl = null;
    f.status = 'idle';
    stopTick();
    renderFormat();
    changed();
  }

  // Why step 3 is not complete ('' when the SYLLABUS.md of the current raw
  // text is saved).
  function formatIncomplete() {
    if (S.fmt.status === 'running') return t('wizard.format.why.running');
    if (!S.md.trim()) return t('wizard.format.why.not_formatted');
    if (S.fmt.rev !== S.rawRev) return t('wizard.format.why.raw_changed');
    if (S.md !== S.savedMd) return t('wizard.format.why.unsaved');
    return '';
  }

  // save() → the updated Course, or null (nothing saved; reason shown).
  async function save() {
    const md = S.md;
    if (!md.trim()) {
      toast(t('wizard.format.empty_md'), 'warn');
      return null;
    }
    const course = await api.saveSyllabus(courseId(), md);
    if (!alive) return course;
    S.savedMd = md;
    S.fmt.rev = S.rawRev; // what was saved is now the SYLLABUS.md of this raw text
    renderFormat();
    changed();
    return course;
  }

  function setDoc(doc) {
    S.doc = { filename: (doc && doc.filename) || '', raw: (doc && doc.raw) || '', markdown: (doc && doc.markdown) || '' };
    S.md = S.doc.markdown;
    S.savedMd = S.doc.markdown;
    S.rawRev = S.doc.raw ? 1 : 0;
    S.rawFrom = S.doc.raw ? 'server' : null;
    S.fmt.rev = S.rawRev; // an existing document is never re-formatted without asking
    S.fmt.base = S.md;
    if (R.editor) {
      R.editor.value = S.md;
      R.editorFresh = true;
    }
  }

  // Reasons why closing now would lose something.
  function dirty() {
    const out = [];
    if (S.mode === 'paste' && S.text.trim() && (S.rawFrom !== 'paste' || S.text !== S.sentText)) out.push(t('wizard.syllabus.why.paste_unsent'));
    if (S.upload && S.upload.status === 'uploading') out.push(t('wizard.syllabus.why.uploading'));
    if (S.fmt.status === 'running') out.push(t('wizard.format.why.running_close'));
    if (S.formatShown && S.md.trim() && S.md !== S.savedMd) out.push(t('wizard.format.why.unsaved'));
    return out;
  }

  function destroy() {
    alive = false;
    S.fmt.seq++;
    if (S.fmt.ctl) S.fmt.ctl.abort();
    life.abort();
    stopTick();
    if (S.upload && S.upload.status === 'uploading' && S.upload.abort) S.upload.abort();
  }

  return {
    S,
    sourceView,
    formatView,
    commitSource,
    sourceIncomplete,
    formatIncomplete,
    ensureFormatted,
    runFormat,
    regenerate,
    cancelFormat,
    save,
    setDoc,
    pickFile,
    dirty,
    destroy,
    busy: () => S.fmt.status === 'running',
    hasMarkdown: () => !!S.md.trim(),
    unsaved: () => !!S.md.trim() && S.md !== S.savedMd,
    needsFormat: () => S.rawRev > 0 && S.fmt.rev !== S.rawRev,
  };
}

// ---------- the wizard ----------

// Text keys of the step rail (label, hint) and of each step's header
// (title, description); resolved with t() at render time.
const STEPS = [
  { label: 'wizard.step.basics', hint: 'wizard.step.basics_hint' },
  { label: 'wizard.step.syllabus', hint: 'wizard.step.syllabus_hint' },
  { label: 'wizard.step.format', hint: 'wizard.step.format_hint' },
  { label: 'wizard.step.materials', hint: 'wizard.step.materials_hint' },
  { label: 'wizard.step.done', hint: 'wizard.step.done_hint' },
];

const STEP_TEXT = [
  ['wizard.step.basics', 'wizard.basics.desc'], // v2 P13.4 adds the term start
  ['wizard.step.syllabus', 'wizard.syllabus.desc'],
  ['wizard.step.format', 'wizard.format.desc'],
  ['wizard.step.materials', 'wizard.materials.desc_week'],
  ['wizard.done.title', 'wizard.done.desc'],
];

export function openWizard(ctx) {
  const existing = document.querySelector('dialog.wiz-add');
  if (existing) {
    existing.querySelector('.wiz-title')?.focus();
    return;
  }
  const W = {
    step: 1,
    visited: 1,
    course: null,
    name: '',
    code: '',
    termStart: '', // v2 P13.4 (step 1, optional)
    skipWeeks: [],
    busy: false, // a request of the primary action is in flight
    sylSkipped: false,
    materials: [], // {key, item, materialId, status}
    knownMaterials: new Set(),
    lectures: [], // drafts created in step 5 (Lecture objects)
  };
  let alive = true;
  const courseId = () => W.course && W.course.id;
  const flow = syllabusFlow(courseId, {
    allowSkip: true,
    autoFormat: true,
    onChange: () => {
      if (!alive) return;
      renderFoot();
      renderRail(); // which steps can be opened depends on what is saved
    },
  });
  const m = modal('wiz wiz-add', t('wizard.title'), requestClose);

  // ---- skeleton ----
  const rail = h('ol', { class: 'wiz-steps' });
  const railSub = h('p', { class: 'wiz-rail-sub' });
  const eyebrow = h('p', { class: 'wiz-eyebrow' });
  const title = h('h3', { class: 'wiz-title', tabindex: '-1' });
  const desc = h('p', { class: 'wiz-desc' });
  const body = h('div', { class: 'wiz-body' });
  const footLeft = h('div', { class: 'wiz-foot-l' });
  const footRight = h('div', { class: 'wiz-foot-r' });
  m.dlg.append(
    h(
      'div',
      { class: 'wiz-shell' },
      h(
        'aside',
        { class: 'wiz-rail' },
        h('div', { class: 'wiz-rail-head' }, h('span', { class: 'wiz-rail-mark' }, icon('graduation-cap', { size: 16 })), h('div', null, h('h2', { class: 'wiz-rail-title' }, t('wizard.title')), railSub)),
        h('nav', { 'aria-label': t('wizard.rail.aria') }, rail),
        h('p', { class: 'wiz-rail-foot' }, h('kbd', null, U.modKey()), '+', h('kbd', null, 'Enter'), ` ${t('wizard.rail.next')} · `, h('kbd', null, 'Esc'), ` ${t('wizard.rail.close')}`),
      ),
      h(
        'section',
        { class: 'wiz-main' },
        h('header', { class: 'wiz-head' }, h('div', { class: 'wiz-head-text' }, eyebrow, title, desc), iconBtn('x', t('wizard.close_aria'), { class: 'wiz-close', onclick: requestClose })),
        body,
        h('footer', { class: 'wiz-foot' }, footLeft, footRight),
      ),
    ),
  );
  m.dlg.addEventListener('keydown', (e) => {
    if (isPrimaryKey(e) && !e.defaultPrevented) {
      e.preventDefault();
      primary();
    }
  });

  // ---- navigation ----
  let stepCleanup = null;
  let stepDrop = null; // what files dropped anywhere on the dialog do in this step

  // Drop targets inside the steps handle their own drops; anything dropped
  // elsewhere on the dialog (rail, header, footer) lands here.
  makeDropTarget(m.dlg, (files) => {
    if (W.busy) toast(t('wizard.saving_wait'), 'info');
    else if (stepDrop) stepDrop(files);
    else toast(W.step === 1 ? t('wizard.drop.step1') : t('wizard.drop.other'), 'info');
  });

  function go(step) {
    if (stepCleanup) stepCleanup();
    stepCleanup = null;
    stepDrop = null;
    if (W.step === 3 && step !== 3) flow.cancelFormat(); // leaving: stop the model call
    W.step = step;
    W.visited = Math.max(W.visited, step);
    render(true);
  }

  // A slide file of step 4 that failed to upload (as the summary counts it).
  const matFailed = (x) => x.status !== 'done' && x.item.status === 'failed';
  // Whether terms are being (or will be) extracted: from the syllabus, or
  // from the slides added in step 4 that did not fail to upload (one still
  // on its way is extracted once it is in; R74: not the failed ones).
  // Without either, the last step does not say that extraction runs in
  // the background (T25 G10).
  const termsComing = () => !!(W.course && W.course.syllabus) || W.materials.some((x) => !matFailed(x));
  const stepDesc = () => t(W.step === 5 && !termsComing() ? 'wizard.done.desc_no_terms' : STEP_TEXT[W.step - 1][1]);

  function render(focus) {
    const [ttl] = STEP_TEXT[W.step - 1];
    eyebrow.textContent = t('wizard.step_of', { n: W.step, total: STEPS.length });
    title.textContent = t(ttl);
    desc.textContent = stepDesc();
    clear(body);
    body.dataset.step = String(W.step);
    body.append(stepView());
    body.scrollTop = 0;
    m.dlg.toggleAttribute('data-drop', !!stepDrop);
    renderRail();
    renderFoot();
    if (focus) {
      const target = body.querySelector('[autofocus]') || title;
      target.focus({ preventScroll: true });
    }
  }

  const railSkipped = () => W.sylSkipped && W.visited > 3;
  // the CoursePatch of the calendar fields against the created course (P13.4)
  const calPatch = () => {
    const c = W.course || {};
    const p = {};
    if (W.termStart !== (c.term_start || '')) p.term_start = W.termStart;
    const old = [...(c.skip_weeks || [])].sort();
    if (JSON.stringify(old) !== JSON.stringify([...W.skipWeeks].sort())) p.skip_weeks = [...W.skipWeeks].sort();
    return p;
  };
  const basicsDirty = () => !!W.course && (W.name.trim() !== W.course.name || W.code.trim() !== (W.course.code || '') || Object.keys(calPatch()).length > 0);

  // Why step k is not complete ('' = it is): moving forward past it would
  // leave something unsaved behind.
  function stepIncomplete(k) {
    if (k === 1) return !W.course ? t('wizard.why.no_course') : basicsDirty() ? t('wizard.why.basics_unsaved') : '';
    if (k === 2) return flow.sourceIncomplete();
    if (k === 3) return flow.S.mode === 'skip' ? '' : flow.formatIncomplete();
    return '';
  }

  // Why step n cannot be opened now: '' = it can; 'todo' = not reached yet
  // (no reason shown); otherwise the reason (lead's ruling on the rail).
  function stepBlock(n) {
    if (n === W.step) return '';
    if (W.busy) return t('wizard.saving_wait');
    if (n === 3 && railSkipped()) return t('wizard.why.syllabus_skipped');
    if (n < W.step) return '';
    if (n > W.visited) return 'todo';
    if (n === 3 && W.step === 2 && flow.S.mode === 'skip') return t('wizard.why.skip_chosen');
    for (let k = W.step; k < n; k++) {
      const why = stepIncomplete(k);
      if (why) return why;
    }
    return '';
  }

  function jump(n) {
    if (stepBlock(n)) return;
    // passing step 2 forward: what it shows is saved (stepBlock), so its choice stands
    if (W.step <= 2 && n > 2) W.sylSkipped = flow.S.mode === 'skip';
    go(n);
  }

  function renderRail() {
    railSub.textContent = W.course ? (W.course.code ? `${W.course.code} · ${W.course.name}` : W.course.name) : t('wizard.new_course');
    preserveFocus(() => {
      clear(rail);
      STEPS.forEach((s, i) => {
        const n = i + 1;
        const skipped = n === 3 && railSkipped();
        // done: behind the current step; visited: ahead of it but seen before
        const state = n === W.step ? 'current' : skipped ? 'skipped' : n < W.step ? 'done' : n <= W.visited ? 'visited' : 'todo';
        const block = stepBlock(n);
        const open = n !== W.step && !block;
        const why = block && block !== 'todo' ? block : '';
        rail.append(
          h(
            'li',
            null,
            h(
              'button',
              {
                type: 'button',
                class: `wiz-step is-${state}` + (why && n > W.step && !skipped ? ' is-blocked' : ''),
                dataset: { fkey: `wiz:step:${n}` },
                'aria-current': n === W.step ? 'step' : undefined,
                // never reached: not focusable; otherwise focusable, and a
                // blocked step tells why when activated
                disabled: state === 'todo',
                'aria-disabled': open || n === W.step || state === 'todo' ? undefined : 'true',
                title: open ? t(n < W.step ? 'wizard.rail.back_to' : 'wizard.rail.go_to', { step: t(s.label) }) : why || undefined,
                onclick: () => {
                  if (open) jump(n);
                  else if (why) toast(t('wizard.rail.blocked', { step: t(s.label), reason: why }), 'info');
                },
              },
              h('span', { class: 'wiz-step-n' }, state === 'done' ? icon('check', { size: 13, stroke: 2.6 }) : String(n)),
              h('span', { class: 'wiz-step-text' }, h('span', { class: 'wiz-step-label' }, t(s.label)), h('span', { class: 'wiz-step-hint' }, skipped ? t('wizard.rail.skipped') : why && n > W.step ? why : t(s.hint))),
            ),
          ),
        );
      });
    });
  }

  function back(n) {
    if (W.busy) return;
    go(n === 3 && W.sylSkipped ? 2 : n);
  }

  function renderFoot() {
    preserveFocus(() => {
      clear(footLeft);
      clear(footRight);
      if (W.step > 1) footLeft.append(btn(t('wizard.foot.back'), { icon: 'arrow-left', disabled: W.busy, fkey: 'wiz:back', onclick: () => back(W.step === 4 && W.sylSkipped ? 2 : W.step - 1) }));
      const p = primarySpec();
      if (p.note) footRight.append(h('span', { class: 'wiz-foot-note' }, p.note));
      footRight.append(spinIfBusy(btn(p.label, { kind: 'primary', icon: p.icon, disabled: p.disabled || W.busy, fkey: 'wiz:primary', onclick: primary, title: `${U.modKey()}+Enter` }), W.busy));
    });
  }

  function primarySpec() {
    switch (W.step) {
      case 1:
        return { label: W.busy ? (W.course ? t('wizard.foot.saving') : t('wizard.foot.creating')) : W.course ? t('wizard.foot.save_next') : t('wizard.foot.create_next'), icon: W.busy ? 'loader-circle' : 'chevron-right', disabled: !W.name.trim() || !!codeError(W.code.trim()) || !!nameError(W.name.trim()) };
      case 2: {
        const mode = flow.S.mode;
        if (mode === 'skip') return { label: t('wizard.foot.skip_next'), icon: 'chevron-right' };
        const uploading = mode === 'upload' && flow.S.upload && flow.S.upload.status === 'uploading';
        return { label: W.busy ? t('wizard.foot.saving_raw') : t('wizard.foot.next_format'), icon: W.busy ? 'loader-circle' : 'sparkles', disabled: !mode || uploading || (mode === 'paste' && !flow.S.text.trim()) || (mode === 'upload' && !(flow.S.upload && flow.S.upload.status === 'done')) };
      }
      case 3:
        return {
          label: W.busy ? t('wizard.foot.saving') : flow.unsaved() ? t('wizard.foot.save_syllabus_next') : t('wizard.foot.next'),
          icon: W.busy ? 'loader-circle' : 'check',
          disabled: flow.busy() || !flow.hasMarkdown(),
          note: flow.busy() ? t('wizard.foot.note_formatting') : flow.unsaved() ? t('wizard.foot.note_extract') : '',
        };
      case 4: {
        const uploading = W.materials.filter((x) => x.status !== 'done' && x.status !== 'failed').length;
        return { label: W.materials.length ? t('wizard.foot.next') : t('wizard.foot.skip_next'), icon: 'chevron-right', note: uploading ? t('wizard.foot.note_uploading', { n: uploading }) : '' };
      }
      default:
        return { label: t('wizard.foot.finish'), icon: 'check' };
    }
  }

  async function primary() {
    if (W.busy) return;
    const p = primarySpec();
    if (p.disabled) {
      if (W.step === 1 && !W.name.trim()) {
        toast(t('wizard.basics.name_required'), 'warn');
        body.querySelector('input')?.focus();
      }
      return;
    }
    if (W.step === 1) return withBusy(saveBasics);
    if (W.step === 2) return withBusy(async () => {
      const r = await flow.commitSource();
      if (r === 'skip') {
        W.sylSkipped = true;
        go(4);
      } else if (r === 'format') {
        W.sylSkipped = false;
        go(3);
      }
    });
    if (W.step === 3) return withBusy(async () => {
      if (flow.unsaved()) {
        const c = await flow.save();
        if (!c) return;
        W.course = c;
        toast(t('wizard.syllabus_saved'), 'success');
      }
      go(4);
    });
    if (W.step === 4) return go(5);
    finish(true);
  }

  async function withBusy(fn) {
    W.busy = true;
    renderFoot();
    renderRail();
    try {
      await fn();
    } catch {
      // the API layer showed the server's error (aborted requests: closed); stay on this step
    } finally {
      W.busy = false;
      if (alive) {
        renderFoot();
        renderRail();
      }
    }
  }

  async function saveBasics() {
    const name = W.name.trim();
    const code = W.code.trim();
    if (!W.course) {
      const c = await api.createCourse(name, code || undefined);
      if (!alive || !c) return;
      W.course = c;
      for (const mat of c.materials || []) W.knownMaterials.add(mat.id);
      ctx.refreshCourses && ctx.refreshCourses().catch(() => {});
      // the term start and skipped weeks are not part of POST /api/courses
      const cp = calPatch();
      if (Object.keys(cp).length) {
        const c2 = await api.patchCourse(c.id, cp);
        if (!alive || !c2) return;
        W.course = c2;
      }
    } else {
      const p = calPatch();
      if (name !== W.course.name) p.name = name;
      if (code !== (W.course.code || '')) p.code = code;
      if (Object.keys(p).length) {
        const c = await api.patchCourse(W.course.id, p);
        if (!alive || !c) return;
        W.course = c;
        ctx.refreshCourses && ctx.refreshCourses().catch(() => {});
      }
    }
    go(2);
  }

  // ---- steps ----

  function stepView() {
    switch (W.step) {
      case 1:
        return basicsView();
      case 2:
        stepDrop = flow.pickFile;
        return flow.sourceView();
      case 3: {
        const v = flow.formatView();
        flow.ensureFormatted();
        return v;
      }
      case 4:
        return materialsView();
      default:
        return doneView();
    }
  }

  function basicsView() {
    const nameErr = h('span', { class: 'field-error', role: 'alert' });
    const codeErr = h('span', { class: 'field-error', role: 'alert' });
    // Duplicates are allowed (no unique constraint); a soft hint says so.
    const nameDup = h('span', { class: 'field-hint is-warn', role: 'status' });
    const codeDup = h('span', { class: 'field-hint is-warn', role: 'status' });
    const others = () => (ctx.courses ? ctx.courses() : []).filter((c) => !W.course || c.id !== W.course.id);
    const same = (a, b) => a.trim().toLowerCase() === b.trim().toLowerCase();
    const ref = h('code', { class: 'cite-ref' });
    const name = h('input', { class: 'input input-lg', value: W.name, maxlength: String(NAME_MAX + 20), placeholder: t('wizard.basics.name_placeholder'), autocomplete: 'off', autofocus: true, 'aria-required': 'true', id: 'wiz-name' });
    const code = h('input', { class: 'input input-lg input-code', value: W.code, maxlength: String(CODE_MAX + 8), placeholder: t('wizard.basics.code_placeholder'), autocomplete: 'off', spellcheck: 'false', id: 'wiz-code' });
    const cal = calendarFields({
      termStart: W.termStart,
      skipWeeks: W.skipWeeks,
      idPrefix: 'wiz-cal',
      onChange: () => {
        const v = cal.value();
        W.termStart = v.term_start;
        W.skipWeeks = v.skip_weeks;
        renderFoot();
        renderRail();
      },
    });
    const sync = () => {
      W.name = name.value;
      W.code = code.value;
      const n = W.name.trim();
      const c = W.code.trim();
      nameErr.textContent = nameError(n);
      codeErr.textContent = codeError(c);
      const dupName = n && !nameErr.textContent ? others().find((o) => same(o.name || '', n)) : null;
      const dupCode = c && !codeErr.textContent ? others().find((o) => o.code && same(o.code, c)) : null;
      nameDup.textContent = dupName ? (dupName.code ? t('wizard.basics.dup_name_code', { code: dupName.code }) : t('wizard.basics.dup_name')) : '';
      codeDup.textContent = dupCode ? t('wizard.basics.dup_code', { code: c, name: dupCode.name }) : '';
      code.setAttribute('aria-invalid', String(!!codeErr.textContent));
      ref.textContent = `[${c && !codeError(c) ? c : n ? courseAbbr(n) : 'CSC1003'} W02L1@12:34]`; // v2 P13.6 numbers
      ref.classList.toggle('is-placeholder', !c && !n);
      renderFoot();
      renderRail();
    };
    name.addEventListener('input', sync);
    code.addEventListener('input', sync);
    sync();
    return h(
      'form',
      {
        class: 'wiz-form',
        onsubmit: (e) => {
          e.preventDefault();
          primary();
        },
      },
      h('div', { class: 'field' }, h('label', { class: 'field-label', for: 'wiz-name' }, t('wizard.basics.name'), h('span', { class: 'field-req' }, t('wizard.basics.required'))), name, nameErr, nameDup),
      h(
        'div',
        { class: 'field' },
        h('label', { class: 'field-label', for: 'wiz-code' }, t('wizard.basics.code'), h('span', { class: 'field-opt' }, t('calendar.optional'))),
        code,
        h('span', { class: 'field-hint' }, t('wizard.basics.code_hint')),
        codeErr,
        codeDup,
      ),
      h('div', { class: 'cite-preview' }, h('span', { class: 'cite-preview-k' }, t('wizard.basics.cite_example')), ref, h('span', { class: 'muted small' }, t('wizard.basics.cite_hint'))),
      // v2 P13.4: the teaching weeks (optional; also in the course settings later)
      h('div', { class: 'wiz-cal' }, h('h4', { class: 'wiz-sub' }, icon('calendar', { size: 15 }), t('wizard.basics.calendar')), h('p', { class: 'field-hint' }, t('wizard.basics.calendar_hint')), cal.el),
      W.course ? h('p', { class: 'wiz-saved' }, icon('circle-check', { size: 14 }), t('wizard.basics.saved', { button: t('wizard.foot.save_next') })) : null,
      h('button', { type: 'submit', hidden: true, tabindex: '-1' }, t('wizard.basics.submit')),
    );
  }

  // Step 4: slides. Uploads go through the global queue (they continue after
  // the wizard closes). lecture kinds: once uploaded, a slide file applies to
  // the teaching week its name gives (Week3, W03; set by the back end) or to
  // the whole course; the select changes it (material_bind.js).
  function materialsView() {
    const list = h('ul', { class: 'wiz-files', 'aria-label': t('wizard.step.materials') });
    const render = () => {
      preserveFocus(() => {
        clear(list);
        for (const x of W.materials) list.append(materialRow(x, render));
      });
    };
    const add = (files) => {
      for (const item of addMaterialFiles(courseId(), files)) W.materials.push({ key: item.key, item, materialId: null, status: item.status });
      render();
      renderFoot();
    };
    stepDrop = add;
    const zone = dropzone({ text: t('wizard.materials.drop'), accept: '.pdf,application/pdf', multiple: true, buttonText: t('wizard.materials.pick'), onFiles: add });
    zone.classList.add('dropzone-lg');
    render();
    const off = onUploadChange((cid, item, result, progressOnly) => {
      if (cid !== courseId() || !alive) return;
      const x = W.materials.find((y) => y.item === item);
      if (!x) return;
      if (result) {
        x.status = 'done';
        const fresh = (result.materials || []).find((mm) => !W.knownMaterials.has(mm.id) && mm.filename === item.file.name);
        for (const mm of result.materials || []) W.knownMaterials.add(mm.id);
        if (fresh) x.materialId = fresh.id;
        W.course = result;
      } else if (!uploadItems(cid, 'material').includes(item)) {
        W.materials = W.materials.filter((y) => y !== x); // removed or cancelled
      } else {
        x.status = item.status;
      }
      if (progressOnly) {
        const row = list.querySelector(`[data-up="${CSS.escape(item.key)}"]`);
        if (row) {
          syncMaterialRow(row, x);
          return;
        }
      }
      render();
      renderFoot();
    });
    stepCleanup = off;
    return h(
      'div',
      { class: 'wiz-materials' },
      zone,
      list,
      h('p', { class: 'wiz-legend wiz-bind-hint' }, icon('calendar', { size: 14 }), t('wizard.materials.bind_hint')),
    );
  }

  // Where an uploaded slide file applies (the server's binding; changed here).
  function bindCell(x, rerender) {
    const mm = x.materialId && (W.course.materials || []).find((y) => y.id === x.materialId);
    if (!mm) return h('span', { class: 'muted small wiz-bind-wait' }, t('wizard.materials.bind_after_upload'));
    return bindSelect(mm, {
      lectures: W.lectures,
      weeks: weekChoices(W.lectures, W.course.materials),
      label: t('wizard.materials.scope_aria', { name: mm.filename }),
      fkey: `wiz:mbind:${mm.id}`,
      onChange: (b) => setMaterialBind(mm, b, rerender),
    });
  }

  async function setMaterialBind(mm, b, rerender) {
    try {
      const updated = await api.setMaterialBind(mm.id, b);
      if (updated && alive) {
        const i = W.course.materials.findIndex((y) => y.id === updated.id);
        if (i >= 0) W.course.materials[i] = updated;
      }
    } catch {
      // shown by the API layer
    }
    if (alive) rerender(); // the server's value
  }

  function materialRow(x, rerender) {
    const it = x.item;
    const seg = h('span', { class: 'wiz-scope' }, bindCell(x, rerender));
    const actions = [];
    if (it.status === 'failed') actions.push(btn(t('common.retry'), { size: 'sm', onclick: () => retryUpload(it) }));
    if (x.status !== 'done') actions.push(iconBtn('x', it.status === 'uploading' ? t('wizard.upload.cancel_named', { name: it.file.name }) : t('wizard.upload.remove_named', { name: it.file.name }), { onclick: () => removeUpload(it) }));
    const row = h(
      'li',
      { class: 'wiz-file', dataset: { up: it.key } },
      h('span', { class: 'wiz-file-icon' }, icon('file-text', { size: 16 })),
      h('div', { class: 'wiz-file-main' }, h('span', { class: 'fname', text: it.file.name, title: it.file.name }), h('span', { class: 'wiz-file-status' }), h('progress', { class: 'up-progress', max: '1', 'aria-label': it.file.name })),
      seg,
      h('span', { class: 'wiz-file-actions' }, actions),
    );
    syncMaterialRow(row, x);
    return row;
  }

  function syncMaterialRow(row, x) {
    const it = x.item;
    const st = row.querySelector('.wiz-file-status');
    const prog = row.querySelector('progress');
    row.className = `wiz-file is-${x.status}`;
    const size = U.fmtBytes(it.file.size);
    if (isLocalFile(it.file)) {
      // desktop app: a local copy has no byte progress (SPEC-v3 V1.4)
      if (x.status === 'done') st.textContent = t('common.native.material_copied');
      else if (it.status === 'failed') st.textContent = t('wizard.upload.failed', { error: it.error });
      else if (it.status === 'waiting') st.textContent = t('common.native.waiting');
      else st.textContent = t('common.native.copying');
    } else if (x.status === 'done') st.textContent = t('wizard.materials.uploaded', { size });
    else if (it.status === 'failed') st.textContent = t('wizard.upload.failed', { error: it.error });
    else if (it.status === 'waiting') st.textContent = t('wizard.upload.waiting', { size });
    else st.textContent = it.fraction == null ? t('wizard.upload.uploading', { size }) : t('wizard.upload.progress', { size, pct: U.pct(it.fraction) });
    prog.hidden = x.status === 'done' || it.status === 'failed';
    if (it.status === 'uploading' && it.fraction != null) prog.value = it.fraction;
    else if (it.status === 'uploading') prog.removeAttribute('value');
    else prog.value = 0;
  }

  // Step 5: summary, the background term extraction (followed through SSE
  // while the step is open), lecture drops (drafts) and binding the
  // "bind later" slides.
  function doneView() {
    const summary = h('div', { class: 'wiz-summary' });
    const note = h('div', { class: 'wiz-sum-note', role: 'status' });
    const lecUploads = h('ul', { class: 'upload-list wiz-uploads' });
    const lecList = h('ul', { class: 'wiz-lecs' });
    const bindBox = h('div', { class: 'wiz-bind' });
    let refreshError = '';
    let bindKey = null;
    // rebuilt on every refresh; focus on its buttons survives (data-fkey)
    const renderSummary = () => preserveFocus(drawSummary);
    const drawSummary = () => {
      const c = W.course;
      clear(summary);
      const gStage = (c.stages || []).find((s) => s.stage === 'glossary_seed');
      let gText = t('wizard.done.seed_none');
      let gTone = 'muted';
      let gIcon = 'circle-check';
      let gAction = null;
      if (gStage) {
        const sum = U.lectureSummary([gStage], {});
        // the stage's own allowed_actions decide whether Retry is offered (§9.8)
        const retry = (gStage.allowed_actions || []).includes('retry') ? btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', fkey: 'wiz:seed-retry', onclick: retrySeed }) : null;
        if (gStage.status === 'done') {
          gText = t('wizard.done.seed_done', { n: c.term_count });
          gTone = 'ok';
        } else if (gStage.status === 'failed') {
          const err = U.stageErrorText(gStage);
          gText = err ? t('wizard.done.seed_failed', { status: sum.text, error: U.excerpt(err, 160) }) : sum.text;
          gTone = 'err';
          gIcon = 'circle-alert';
          gAction = retry;
        } else if (gStage.status === 'cancelled') {
          gText = U.statusName('cancelled');
          gIcon = 'circle-alert';
          gAction = retry;
        } else {
          gText = gStage.status === 'running' ? t('wizard.done.seed_running', { n: c.term_count }) : t('wizard.done.seed_queued');
          gTone = 'run';
          gIcon = 'loader-circle';
        }
      } else if (c.syllabus) {
        gText = t('wizard.done.seed_waiting');
      }
      const mats = W.materials.filter((x) => x.status === 'done').length;
      const failed = W.materials.filter(matFailed).length;
      const pending = W.materials.length - mats - failed;
      const matText = [mats ? t('wizard.done.mats_uploaded', { n: mats }) : '', pending ? t('wizard.done.mats_uploading', { n: pending }) : '', failed ? t('wizard.done.mats_failed', { n: failed }) : '']
        .filter(Boolean)
        .join(t('wizard.done.sep'));
      summary.append(
        summaryItem('graduation-cap', t('wizard.done.k_course'), c.code ? `${c.code} · ${c.name}` : c.name, 'ok'),
        summaryItem(
          'file-text',
          t('wizard.done.k_syllabus'),
          c.syllabus && c.syllabus.formatted
            ? t('wizard.done.syllabus_chars', { n: Number(c.syllabus.chars), count: U.fmtInt(c.syllabus.chars) })
            : W.sylSkipped
              ? t('wizard.done.syllabus_skipped')
              : t('wizard.done.syllabus_none'),
          c.syllabus ? 'ok' : 'muted',
        ),
        summaryItem('layers', t('wizard.done.k_materials'), matText || t('wizard.done.materials_none'), failed ? 'err' : W.materials.length ? 'ok' : 'muted'),
        summaryItem(gIcon, t('wizard.done.k_terms'), gText, gTone, gIcon === 'loader-circle', gAction),
      );
      clear(note);
      if (refreshError) note.append(icon('circle-alert', { size: 14 }), h('span', null, t('wizard.done.refresh_failed', { error: refreshError })), btn(t('common.retry'), { size: 'sm', kind: 'ghost', icon: 'rotate-cw', fkey: 'wiz:refresh', onclick: refresh }));
    };
    // Retry of a failed or cancelled term extraction; the server's Course
    // answer is shown. Sent once while it is on its way (a double click,
    // 3.2.0 P2).
    let seeding = false;
    async function retrySeed() {
      if (seeding) return;
      seeding = true;
      try {
        const c = await api.courseStageAction(W.course.id, 'glossary_seed', 'retry');
        if (!alive || !c) return;
        W.course = c;
        renderSummary();
        renderRail();
      } catch {
        // shown by the API layer
      } finally {
        seeding = false;
      }
    }
    const renderLectures = () => {
      clear(lecUploads);
      for (const it of uploadItems(courseId(), 'lecture')) lecUploads.append(uploadRow(it, { onRetry: retryUpload, onRemove: removeUpload }));
      clear(lecList);
      for (const l of W.lectures) lecList.append(h('li', null, kindTag(l, { class: 'ktag-inline' }), h('span', { class: 'wiz-lec-title', title: l.title }, l.title), h('span', { class: 'chip chip-draft' }, t('lecture.page.draft'))));
      renderBind();
    };
    // Where the slides apply, now that lectures can be added (lecture kinds:
    // a teaching week, one lecture, or the whole course). Rebuilt only when
    // the slides, their bindings or the lectures change, so an SSE refresh
    // never closes a <select> the user has open.
    const renderBind = () => {
      const mats = (W.course.materials || []).filter((mm) => W.materials.some((x) => x.materialId === mm.id));
      const key = JSON.stringify([mats.map((mm) => [mm.id, mm.bind || null, mm.lecture_id || '']), W.lectures.map((l) => [l.id, l.kind, l.week, l.title])]);
      if (key === bindKey) return;
      bindKey = key;
      preserveFocus(() => {
        clear(bindBox);
        if (!mats.length) return;
        bindBox.append(h('h4', { class: 'wiz-sub' }, icon('layers', { size: 14 }), t('wizard.done.bind_scope_title')));
        bindBox.append(
          h(
            'ul',
            { class: 'wiz-bind-list' },
            mats.map((mm) =>
              h(
                'li',
                null,
                icon('file-text', { size: 14, class: 'dim' }),
                h('span', { class: 'fname', text: mm.filename, title: mm.filename }),
                bindSelect(mm, {
                  lectures: W.lectures,
                  weeks: weekChoices(W.lectures, W.course.materials),
                  label: t('wizard.done.bind_aria', { name: mm.filename }),
                  fkey: `wiz:bind:${mm.id}`,
                  onChange: (b) =>
                    setMaterialBind(mm, b, () => {
                      bindKey = null; // show the server's value
                      renderBind();
                      renderSummary();
                    }),
                }),
              ),
            ),
          ),
        );
      });
    };
    const addLectures = (files) => addLectureFiles(courseId(), files).then(() => alive && renderLectures());
    stepDrop = addLectures;
    const drop = h(
      'div',
      { class: 'wiz-lec-drop' },
      dropzone({ text: t('wizard.done.drop'), accept: MEDIA_ACCEPT, multiple: true, buttonText: t('wizard.done.pick'), target: false, onFiles: addLectures }),
    );
    makeDropTarget(drop, addLectures);
    renderSummary();
    renderLectures();
    const off = onUploadChange((cid, item, result, progressOnly) => {
      if (cid !== courseId() || !alive) return;
      if (item.kind === 'lecture') {
        if (progressOnly && updateUploadRow(lecUploads, item)) return;
        if (result) W.lectures.push(result);
        renderLectures();
      } else if (item.kind === 'material' && result) {
        W.course = result;
        const x = W.materials.find((y) => y.item === item);
        if (x) {
          x.status = 'done';
          const fresh = (result.materials || []).find((mm) => !W.knownMaterials.has(mm.id) && mm.filename === item.file.name);
          if (fresh) x.materialId = fresh.id;
        }
        for (const mm of result.materials || []) W.knownMaterials.add(mm.id);
        renderSummary();
        renderBind();
      } else if (item.kind === 'material' && !progressOnly && W.materials.some((y) => y.item === item)) {
        // one of step 4's slide files failed (or was taken up again) while
        // this step is open: its count, and whether terms are still coming
        renderSummary();
        desc.textContent = stepDesc();
      }
    });
    // Term and slide extraction run in the background: follow them through
    // the app's SSE stream and re-read the course (debounced, quiet: a
    // failure is shown under the summary and the next event tries again).
    let ctl = null;
    let timer = null;
    async function refresh() {
      clearTimeout(timer);
      timer = null;
      if (ctl) ctl.abort();
      const mine = new AbortController();
      ctl = mine;
      try {
        const c = await api.course(courseId(), { quiet: true, signal: mine.signal });
        if (!alive || ctl !== mine) return;
        if (c) W.course = c;
        refreshError = '';
      } catch (e) {
        if (!alive || ctl !== mine || isAbort(e)) return;
        refreshError = (e && e.message) || t('common.load_failed');
      } finally {
        if (ctl === mine) ctl = null;
      }
      renderSummary();
      renderBind();
      renderRail();
    }
    const soon = () => {
      clearTimeout(timer);
      timer = setTimeout(refresh, REFRESH_MS);
    };
    const unsubscribe = ctx.subscribe
      ? ctx.subscribe((type, ev) => {
          if (!alive || !W.course) return;
          if (type === 'reconnected') soon(); // changes during the outage
          if (!ev) return;
          if (type === 'job') {
            if ((ev.scope === 'course' && ev.scope_id === W.course.id) || (ev.scope === 'material' && (W.course.materials || []).some((mm) => mm.id === ev.scope_id))) soon();
          } else if (type === 'changed') {
            if ((ev.kind === 'course' || ev.kind === 'terms') && ev.id === W.course.id) soon();
          }
        })
      : null;
    refresh();
    stepCleanup = () => {
      off();
      if (unsubscribe) unsubscribe();
      clearTimeout(timer);
      if (ctl) ctl.abort();
    };
    return h(
      'div',
      { class: 'wiz-done' },
      summary,
      note,
      h('h4', { class: 'wiz-sub' }, icon('audio-lines', { size: 14 }), t('wizard.done.add_lectures'), h('span', { class: 'muted small' }, t('wizard.done.add_lectures_hint'))),
      drop,
      lecUploads,
      lecList,
      bindBox,
    );
  }

  function summaryItem(ic, k, v, tone, spin = false, action = null) {
    return h(
      'div',
      { class: `wiz-sum tone-${tone}` },
      h('span', { class: 'wiz-sum-icon' }, icon(ic, { size: 16, class: spin ? 'spin' : '' })),
      h('div', { class: 'wiz-sum-text' }, h('span', { class: 'wiz-sum-k' }, k), h('span', { class: 'wiz-sum-v' }, v), action ? h('span', { class: 'wiz-sum-act' }, action) : null),
    );
  }

  // ---- closing ----

  async function requestClose() {
    if (W.busy && W.step === 1) return; // creating the course: wait for the answer
    const reasons = [];
    if (!W.course && (W.name.trim() || W.code.trim())) reasons.push(t('wizard.why.no_course'));
    if (W.course && W.step === 1 && basicsDirty()) reasons.push(t('wizard.close.basics_unsaved'));
    reasons.push(...flow.dirty());
    if (reasons.length) {
      const extra = W.course ? t('wizard.close.created', { name: W.course.name }) : '';
      if (!(await lostWorkDialog(reasons, extra))) return;
    }
    finish(!!W.course);
  }

  function finish(openCourse) {
    if (!alive) return;
    alive = false;
    if (stepCleanup) stepCleanup();
    stepCleanup = null;
    flow.destroy();
    m.close();
    if (W.course) {
      ctx.refreshCourses && ctx.refreshCourses().catch(() => {});
      if (openCourse) location.hash = `#/course/${encodeURIComponent(W.course.id)}`;
    }
  }

  m.show();
  render(true);
}

// ---------- the course-page editor (A8.2) ----------

// openSyllabusEditor(course, {ctx, source, files, onSaved}): ctx = the app
// context (its subscribe() keeps the course, and so the glossary_seed
// state, current while the editor is open); source = start at the raw-text
// picker ("replace raw text"); files = a dropped file to upload right away.
export function openSyllabusEditor(course, opts = {}) {
  const existing = document.querySelector('dialog.syl-dlg');
  if (existing) return;
  const live = !!(opts.ctx && opts.ctx.subscribe);
  let alive = true;
  let phase = 'loading'; // loading | error | source | format
  let loadError = '';
  let busy = false;
  let hasDoc = false;
  const life = new AbortController(); // aborted when the editor closes
  const flow = syllabusFlow(() => course.id, { allowSkip: false, autoFormat: false, onChange: () => alive && renderFoot() });
  const m = modal('wiz syl-dlg', t('wizard.editor.title'), requestClose);
  const title = h('h3', { class: 'wiz-title', tabindex: '-1' });
  const desc = h('p', { class: 'wiz-desc' });
  const body = h('div', { class: 'wiz-body' });
  const footLeft = h('div', { class: 'wiz-foot-l' });
  const footRight = h('div', { class: 'wiz-foot-r' });
  m.dlg.append(
    h(
      'section',
      { class: 'wiz-main' },
      h(
        'header',
        { class: 'wiz-head' },
        h('div', { class: 'wiz-head-text' }, h('p', { class: 'wiz-eyebrow' }, course.code ? h('span', { class: 'mono' }, course.code) : null, course.code ? ' · ' : '', course.name), title, desc),
        iconBtn('x', t('common.close'), { class: 'wiz-close', onclick: requestClose }),
      ),
      body,
      h('footer', { class: 'wiz-foot' }, footLeft, footRight),
    ),
  );
  m.dlg.addEventListener('keydown', (e) => {
    if (isPrimaryKey(e)) {
      e.preventDefault();
      primary();
    }
  });
  // A file dropped anywhere on the editor becomes the new raw text.
  makeDropTarget(m.dlg, (files) => replaceWith(files));

  // glossary_seed of this course: the server refuses a new SYLLABUS.md while
  // it runs (409), so the footer says so and — when the state is followed
  // live — the save waits for it.
  const seedRunning = () => (course.stages || []).some((s) => s.stage === 'glossary_seed' && s.status === 'running');
  let courseTimer = null;
  let courseCtl = null;
  const unsubscribe = live
    ? opts.ctx.subscribe((type, ev) => {
        if (!alive) return;
        const mine = type === 'reconnected' || (ev && ((type === 'job' && ev.scope === 'course' && ev.scope_id === course.id) || (type === 'changed' && ev.kind === 'course' && ev.id === course.id)));
        if (!mine) return;
        clearTimeout(courseTimer);
        courseTimer = setTimeout(refreshCourse, REFRESH_MS);
      })
    : null;

  async function refreshCourse() {
    if (courseCtl) courseCtl.abort();
    const mine = new AbortController();
    courseCtl = mine;
    try {
      const c = await api.course(course.id, { quiet: true, signal: mine.signal });
      if (!alive || courseCtl !== mine || !c) return;
      course = c;
      renderFoot();
    } catch {
      // quiet: the footer keeps the last known state; a save answers 409 if needed
    } finally {
      if (courseCtl === mine) courseCtl = null;
    }
  }

  function render(focus) {
    clear(body);
    if (phase === 'loading') {
      title.textContent = t('wizard.editor.title');
      desc.textContent = '';
      body.append(h('div', { class: 'syl-loading' }, skeleton(8)));
    } else if (phase === 'error') {
      title.textContent = t('wizard.editor.title');
      desc.textContent = '';
      body.append(h('div', { class: 'syl-load-err' }, errorBlock(t('wizard.editor.load_failed', { error: loadError }), load)));
    } else if (phase === 'source') {
      title.textContent = hasDoc ? t('wizard.editor.replace_title') : t('wizard.editor.add_title');
      desc.textContent = t('wizard.editor.source_desc');
      body.append(flow.sourceView());
    } else {
      title.textContent = t('wizard.editor.title');
      desc.textContent = t('wizard.editor.format_desc');
      body.append(flow.formatView());
      flow.ensureFormatted();
    }
    body.dataset.step = phase === 'format' ? '3' : '2';
    m.dlg.toggleAttribute('data-drop', phase === 'source' || phase === 'format');
    renderFoot();
    if (focus) {
      const target = body.querySelector('[autofocus]') || title;
      target.focus({ preventScroll: true });
    }
  }

  function renderFoot() {
    preserveFocus(() => {
      clear(footLeft);
      clear(footRight);
      if (phase === 'format') {
        footLeft.append(btn(t('wizard.editor.replace_raw'), { icon: 'upload', disabled: busy || flow.busy(), fkey: 'syl:source', onclick: () => ((phase = 'source'), render(true)) }));
        const waiting = seedRunning();
        if (waiting) footLeft.append(h('span', { class: 'wiz-foot-note tone-warn', role: 'status' }, icon('loader-circle', { size: 14, class: 'spin' }), t('wizard.editor.seed_running')));
        footRight.append(btn(t('common.cancel'), { fkey: 'syl:cancel', onclick: requestClose }));
        footRight.append(spinIfBusy(btn(busy ? t('wizard.foot.saving') : t('wizard.editor.save'), { kind: 'primary', icon: busy ? 'loader-circle' : 'check', disabled: busy || flow.busy() || !flow.hasMarkdown() || !flow.unsaved() || (live && waiting), fkey: 'syl:save', onclick: primary, title: `${U.modKey()}+Enter` }), busy));
      } else if (phase === 'source') {
        if (hasDoc) footLeft.append(btn(t('wizard.editor.back_to_edit'), { icon: 'arrow-left', disabled: busy, fkey: 'syl:back', onclick: () => ((phase = 'format'), render(true)) }));
        footRight.append(btn(t('common.cancel'), { fkey: 'syl:cancel', onclick: requestClose }));
        const mode = flow.S.mode;
        const ready = (mode === 'paste' && flow.S.text.trim()) || (mode === 'upload' && flow.S.upload && flow.S.upload.status === 'done');
        footRight.append(spinIfBusy(btn(busy ? t('wizard.foot.saving_raw') : t('wizard.foot.next_format'), { kind: 'primary', icon: busy ? 'loader-circle' : 'sparkles', disabled: busy || !ready, fkey: 'syl:next', onclick: primary, title: `${U.modKey()}+Enter` }), busy));
      } else {
        footRight.append(btn(t('common.close'), { fkey: 'syl:cancel', onclick: requestClose }));
      }
    });
  }

  async function primary() {
    if (busy) return;
    if (phase === 'source') {
      busy = true;
      renderFoot();
      try {
        if ((await flow.commitSource()) === 'format') {
          hasDoc = true;
          phase = 'format';
          busy = false;
          render(true);
          // "Next: format" asks for the model (the editor never calls it by itself)
          if (flow.needsFormat()) flow.regenerate();
          return;
        }
      } catch {
        // shown by the API layer (aborted: closed)
      }
      busy = false;
      if (alive) renderFoot();
    } else if (phase === 'format') {
      if (flow.busy() || !flow.unsaved()) return;
      if (live && seedRunning()) {
        toast(t('wizard.editor.seed_running'), 'info');
        return;
      }
      busy = true;
      renderFoot();
      let saved = null;
      try {
        saved = await flow.save();
      } catch {
        // shown by the API layer (e.g. 409 while glossary_seed runs)
      }
      busy = false;
      if (!alive) return;
      if (!saved) {
        renderFoot();
        return;
      }
      toast(t('wizard.editor.saved'), 'success');
      close();
      if (opts.onSaved) opts.onSaved(saved);
    }
  }

  // A dropped file: upload it as the new raw text (the source picker shows
  // the upload, then "Next: format" as usual).
  function replaceWith(files) {
    if (phase === 'source') return flow.pickFile(files);
    if (phase !== 'format') return;
    if (busy || flow.busy()) {
      toast(t('wizard.editor.busy_replace'), 'info');
      return;
    }
    phase = 'source';
    render(false);
    flow.pickFile(files);
  }

  async function load() {
    phase = 'loading';
    render(false);
    try {
      // quiet: a failure is shown in the dialog, with retry
      const doc = await api.syllabus(course.id, { quiet: true, signal: life.signal });
      if (!alive) return;
      flow.setDoc(doc);
      hasDoc = !!(doc && (doc.raw || doc.markdown));
      phase = opts.source || opts.files || !hasDoc ? 'source' : 'format';
      if (phase === 'source') flow.S.mode = opts.files ? 'upload' : 'paste';
      render(true);
      if (opts.files) flow.pickFile(opts.files);
    } catch (e) {
      if (!alive || isAbort(e)) return;
      loadError = (e && e.message) || t('common.unknown_error');
      phase = 'error';
      render(false);
      body.querySelector('.error-block .btn')?.focus({ preventScroll: true });
    }
  }

  async function requestClose() {
    const reasons = flow.dirty();
    if (reasons.length && !(await lostWorkDialog(reasons, ''))) return;
    close();
  }

  function close() {
    if (!alive) return;
    alive = false;
    flow.destroy();
    life.abort();
    if (unsubscribe) unsubscribe();
    clearTimeout(courseTimer);
    if (courseCtl) courseCtl.abort();
    m.close();
  }

  m.show();
  load();
  if (live) refreshCourse(); // the course object handed over may be a little old
}
