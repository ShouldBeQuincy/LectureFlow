// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Which processes belong to one run of the Firefox tests, and which of them
// the run may stop: pure functions over a process table, so the rules are
// tested without a Firefox (test/unit/firefox-run-processes.test.mjs).
//
// A row: { ProcessId, ParentProcessId, Name, CommandLine?, Created? }, Created
// being the creation time when the system gives it, in one unit for all rows
// of a run (firefox.test.mjs: microseconds since the epoch on Windows, whole
// seconds where ps only gives the elapsed time). Only comparisons are made.
//
// Windows never updates the parent id of a process whose parent has exited,
// and ids are reused: a parent id alone does not prove a parent. The user's
// own Firefox is usually such an orphan (its launcher exits). So a process is
// stopped only when it is certainly this run's; whenever something needed
// for that is missing, nothing is stopped.

const baseName = (n) => String(n || '').split(/[\\/]/).pop();

/** A Firefox main or content process by its image name. */
export const isFirefox = (r) => /^firefox(\.exe)?$/i.test(baseName(r.Name));

/**
 * The programs a run of the suite starts: Firefox (main and content
 * processes are firefox.exe on Windows; firefox / firefox-bin elsewhere,
 * content processes plugin-container on some systems), crashhelper (seen
 * outliving Firefox, with this run's directory in its command line), and
 * what Firefox may start on its own during a run: pingsender (telemetry,
 * switched off), crashreporter and minidump-analyzer (after a crash; the
 * reporter is switched off). Nothing else is taken for this run's by its
 * command line.
 */
export const isSuiteProgram = (r) => /^(firefox|firefox-bin|plugin-container|crashhelper|pingsender|crashreporter|minidump-analyzer)(\.exe)?$/i.test(baseName(r.Name));

const known = (t) => typeof t === 'number' && Number.isFinite(t);

/**
 * The processes of this run: [{ ProcessId, ParentProcessId, Name, ours, stop }].
 *   ours: 'main' (the Firefox web-ext reported), 'dirs' (one of the suite's
 *   programs whose command line names one of `dirs`), 'started' (see below),
 *   'tree' (a descendant of one of these).
 *   stop: whether the run may end it.
 *
 * rows        the process table now
 * mainPid     the Firefox web-ext started (0 when the start failed half-way)
 * dirs        directories only this run's processes name (its temporary
 *             directory, Firefox's temp directory, the tests' Firefox copy)
 * selfPid     this test process
 * selfStart   its creation time, from the same table as `preexisting`
 *             (null when unknown)
 * preexisting the ids of all processes that existed before this run started
 *             Firefox (a Set; null when that snapshot could not be taken)
 *
 * 'started' (only when mainPid is 0: the case it is for): a firefox process
 * whose parent id is this process, that did not exist before the run started
 * Firefox, and was created after this process. While this process lives no
 * other process can get its id, so such a process is this process's child; a
 * stale parent id could only be on a process older than this one.
 * A 'tree' process is stopped only below a 'started' one, and only when it too
 * is newer than both that process and this one and was not there before.
 * 'dirs' (with or without a main pid): one of the suite's programs
 * (isSuiteProgram) naming one of `dirs`; anything else naming them (a shell
 * looking at the directory) is not this run's. It is stopped only when it is
 * new (not in the snapshot, created after this process): the copy's
 * directory and Firefox's temp directory are shared with runs from other
 * working copies. When that cannot be told it is listed (the run then fails
 * on it) but not stopped; when it is known to be older, it is not this run's.
 */
export function runProcesses({ rows, mainPid = 0, dirs = [], selfPid, selfStart = null, preexisting = null }) {
  if (!Array.isArray(rows)) return [];
  const named = (r) => dirs.some((d) => d && String(r.CommandLine || '').toLowerCase().includes(String(d).toLowerCase()));
  // fresh: certainly started after this run took its snapshot (by anyone).
  const tellable = (r) => preexisting instanceof Set && known(selfStart) && known(r.Created);
  const fresh = (r) => tellable(r) && r.Created >= selfStart && !preexisting.has(r.ProcessId);
  const isNew = (r) => !mainPid && fresh(r);
  const started = (r) => !mainPid && r.ParentProcessId === selfPid && r.ProcessId !== selfPid && isFirefox(r) && fresh(r);
  const byDirs = (r) => isSuiteProgram(r) && named(r) && (!tellable(r) || fresh(r));

  const kind = new Map(); // pid → 'main' | 'dirs' | 'started' | 'tree'
  const stoppable = new Map(); // pid → the 'started' row it hangs below, for 'tree' rows
  for (const r of rows) {
    if (r.ProcessId === selfPid) continue;
    if (mainPid && r.ProcessId === mainPid) kind.set(r.ProcessId, 'main');
    else if (byDirs(r)) kind.set(r.ProcessId, 'dirs');
    else if (started(r)) kind.set(r.ProcessId, 'started');
  }
  const byPid = new Map(rows.map((r) => [r.ProcessId, r]));
  // Descendants: a child must not be older than its parent (else its parent
  // id is stale and names some other, newer process).
  for (let grew = true; grew;) {
    grew = false;
    for (const r of rows) {
      if (kind.has(r.ProcessId) || r.ProcessId === selfPid || !kind.has(r.ParentProcessId)) continue;
      const parent = byPid.get(r.ParentProcessId);
      if (known(r.Created) && parent && known(parent.Created) && r.Created < parent.Created) continue;
      kind.set(r.ProcessId, 'tree');
      const root = kind.get(r.ParentProcessId) === 'started' ? parent : stoppable.get(r.ParentProcessId);
      if (root && isNew(r) && known(root.Created) && r.Created >= root.Created) stoppable.set(r.ProcessId, root);
      grew = true;
    }
  }
  // Children of a main process that is already gone (listed, never stopped).
  if (mainPid && !byPid.has(mainPid)) {
    for (const r of rows) {
      if (r.ParentProcessId === mainPid && !kind.has(r.ProcessId) && r.ProcessId !== selfPid && !(known(selfStart) && known(r.Created) && r.Created < selfStart)) kind.set(r.ProcessId, 'tree');
    }
  }
  return rows.filter((r) => kind.has(r.ProcessId)).map((r) => {
    const ours = kind.get(r.ProcessId);
    const stop = (ours === 'dirs' && fresh(r)) || ours === 'started' || (ours === 'tree' && stoppable.has(r.ProcessId));
    return { ProcessId: r.ProcessId, ParentProcessId: r.ParentProcessId, Name: r.Name, ours, stop };
  });
}

/** Whether `p` is strictly inside `dir` (both resolved with `pathApi`; never `dir` itself). */
export function isInside(p, dir, pathApi) {
  if (!p || !dir) return false;
  const rel = pathApi.relative(pathApi.resolve(dir), pathApi.resolve(p));
  return rel !== '' && rel !== '..' && !rel.startsWith(`..${pathApi.sep}`) && !pathApi.isAbsolute(rel);
}
