// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

import (
	"os"
	"path/filepath"
	"testing"

	"lectureflow/internal/devices"
	"lectureflow/internal/model"
)

// next-33: a desktop without an accelerator after the recommended install
// (SPEC-v3 V3.4: asr-openvino + small int8). fw:cpu is on by default but
// asr-cuda is not installed: the lecture waits component_missing:fw:cpu
// while ov:CPU is off (the state before next-33, nothing transcribes).
// Once the detection turns ov:CPU on with the installed model
// (devices.applyCPUFallback), the lecture runs there by itself; fw:cpu,
// still on and still without its environment, is skipped.
func TestCPUOnlyDesktopRunsOnOpenVINOCPU(t *testing.T) {
	fw := devRow(model.DeviceFWCPU, 10, 1)
	fw.Model = "small"
	ov := devRow(model.DeviceOVCPU, 0, 1)
	ov.Enabled = false
	de, fc := componentEnv(t, false, fw, ov)
	de.power.set(onAC)
	dir := t.TempDir()
	for _, name := range devices.ModelFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fc.installEnv(model.BackendOpenVINO)
	fc.installModel(model.BackendOpenVINO, "small", dir)
	l := de.lectureOn(autoOn(fw))
	de.prepDone(l)
	de.start()
	de.stillQueued(l, devices.WaitComponentMissing(model.DeviceFWCPU))

	de.devs.set(model.DeviceOVCPU, func(d *model.ASRDevice) {
		d.Enabled, d.Model, d.Precision = true, "small", model.PrecisionINT8
	})
	de.s.Kick()
	de.waitRunningOn(l, model.DeviceOVCPU)
}
