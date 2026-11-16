// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

//go:build windows

package hwdetect

import (
	"context"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"lectureflow/internal/procutil"
)

const createNoWindow = 0x08000000

func platformOptions() Options {
	return Options{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Run:        runHidden,
		DiskFreeMB: diskFreeMB,
		TotalRAMMB: totalRAMMB,
	}
}

// runHidden runs a probe without flashing a console window (the desktop
// app's service has no console), in the probe's own directory, not the
// service's current one (procutil.CommandInExeDir; SEC52 info-7).
func runHidden(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := procutil.CommandInExeDir(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGlobalMemoryStatus  = kernel32.NewProc("GlobalMemoryStatusEx")
)

func diskFreeMB(dir string) (int, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	r, _, e := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0, e
	}
	return int(free / bytesPerMB), nil
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func totalRAMMB() (int, error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, e := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, e
	}
	return int(m.TotalPhys / bytesPerMB), nil
}
