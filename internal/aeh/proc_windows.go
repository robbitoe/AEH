//go:build windows

package aeh

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	processKernel32 = syscall.NewLazyDLL("kernel32.dll")
	createJob       = processKernel32.NewProc("CreateJobObjectW")
	assignJob       = processKernel32.NewProc("AssignProcessToJobObject")
	terminateJob    = processKernel32.NewProc("TerminateJobObject")
	queryJob        = processKernel32.NewProc("QueryInformationJobObject")
	threadFirst     = processKernel32.NewProc("Thread32First")
	threadNext      = processKernel32.NewProc("Thread32Next")
	openThread      = processKernel32.NewProc("OpenThread")
	resumeThread    = processKernel32.NewProc("ResumeThread")
)

// runProcessTree assigns the suspended child to a job before it can create any
// descendants. Job membership survives intermediate launchers exiting (MSYS
// shells do this), unlike a process tree reconstructed by taskkill from PIDs.
func runProcessTree(cmd *exec.Cmd) error {
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return fmt.Errorf("create process job: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(job))
	var mu sync.Mutex
	cmd.Cancel = func() error {
		mu.Lock()
		defer mu.Unlock()
		return killJob(job)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x00000004 // CREATE_SUSPENDED
	// CommandContext's cancellation goroutine must wait until assignment and
	// resume finish; otherwise it could terminate an empty job before assignment.
	mu.Lock()
	err = cmd.Start()
	if err == nil {
		err = assignAndResume(job, uint32(cmd.Process.Pid))
	}
	if err != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = killJob(job)
	}
	mu.Unlock()
	if err != nil {
		if cmd.Process != nil {
			_ = cmd.Wait()
		}
		return fmt.Errorf("start process tree: %w", err)
	}
	return cmd.Wait()
}

func assignAndResume(job uintptr, pid uint32) error {
	const processSetQuotaAndTerminate = 0x0100 | 0x0001
	process, err := syscall.OpenProcess(processSetQuotaAndTerminate, false, pid)
	if err != nil {
		return fmt.Errorf("open process %d for job assignment: %w", pid, err)
	}
	defer syscall.CloseHandle(process)
	if ok, _, err := assignJob.Call(job, uintptr(process)); ok == 0 {
		return fmt.Errorf("assign process %d to job: %w", pid, err)
	}
	// os/exec closes CreateProcess's primary thread handle. Enumerate all owned
	// threads: injected DLLs can create others even while the primary is suspended.
	const snapshotThreads = 0x00000004 // TH32CS_SNAPTHREAD
	snapshot, err := syscall.CreateToolhelp32Snapshot(snapshotThreads, 0)
	if err != nil {
		return fmt.Errorf("snapshot process threads: %w", err)
	}
	defer syscall.CloseHandle(snapshot)
	var entry struct {
		Size, Usage, ThreadID, OwnerProcessID uint32
		BasePriority, DeltaPriority           int32
		Flags                                 uint32
	}
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := 0
	for ok, _, callErr := threadFirst.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry))); ; ok, _, callErr = threadNext.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry))) {
		if ok == 0 {
			if !errors.Is(callErr, syscall.ERROR_NO_MORE_FILES) {
				return fmt.Errorf("enumerate threads of process %d: %w", pid, callErr)
			}
			break
		}
		if entry.OwnerProcessID != pid {
			continue
		}
		const threadSuspendResume = 0x0002
		thread, _, err := openThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
		if thread == 0 {
			if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: the thread already exited
				continue
			}
			return fmt.Errorf("open thread %d of process %d: %w", entry.ThreadID, pid, err)
		}
		previous, _, err := resumeThread.Call(thread)
		syscall.CloseHandle(syscall.Handle(thread))
		if previous == 0xffffffff {
			return fmt.Errorf("resume thread %d of process %d: %w", entry.ThreadID, pid, err)
		}
		if previous > 0 {
			resumed++
		}
	}
	if resumed == 0 {
		return fmt.Errorf("no suspended thread resumed for process %d", pid)
	}
	return nil
}

func killJob(job uintptr) error {
	deadline := time.Now().Add(2 * time.Second)
	processes := make(map[uint32]syscall.Handle)
	defer func() {
		for _, process := range processes {
			syscall.CloseHandle(process)
		}
	}()
	// Acquire handles BEFORE termination: a member can leave the active list
	// before its process object is signaled and its log handles are released.
	captureErr := captureJobProcesses(job, deadline, processes)
	if ok, _, err := terminateJob.Call(job, 1); ok == 0 {
		return fmt.Errorf("terminate process job: %w", err)
	}
	if captureErr != nil {
		return captureErr
	}
	for {
		// Include descendants created between the initial snapshot and termination.
		if err := captureJobProcesses(job, deadline, processes); err != nil {
			return err
		}
		for pid, process := range processes {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return fmt.Errorf("process %d did not finish job termination within two seconds", pid)
			}
			status, err := syscall.WaitForSingleObject(process, uint32((remaining+time.Millisecond-1)/time.Millisecond))
			if err != nil {
				return fmt.Errorf("wait for terminated job process %d: %w", pid, err)
			}
			if status != syscall.WAIT_OBJECT_0 {
				return fmt.Errorf("wait for terminated job process %d: status %#x", pid, status)
			}
		}
		var accounting struct {
			UserTime, KernelTime, PeriodUserTime, PeriodKernelTime  int64
			PageFaults, TotalProcesses, ActiveProcesses, Terminated uint32
		}
		const basicAccountingInformation = 1
		if ok, _, err := queryJob.Call(job, basicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), unsafe.Sizeof(accounting), 0); ok == 0 {
			return fmt.Errorf("query terminated process job: %w", err)
		}
		if accounting.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process job still has %d active processes after termination", accounting.ActiveProcesses)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// captureJobProcesses uses JobObjectBasicProcessIdList, growing the buffer if
// the job grows during enumeration. Kept handles identify objects, not reusable
// PIDs. ERROR_INVALID_PARAMETER means that process has already disappeared.
func captureJobProcesses(job uintptr, deadline time.Time, processes map[uint32]syscall.Handle) error {
	capacity := uint32(64)
	for time.Now().Before(deadline) {
		// []uintptr guarantees the alignment of the ULONG_PTR list after the
		// two-DWORD header (8 bytes: one word on 64-bit, two on 32-bit).
		word := int(unsafe.Sizeof(uintptr(0)))
		buffer := make([]uintptr, (8+int(capacity)*word+word-1)/word)
		info := (*struct {
			Assigned, Count uint32
			PIDs            [1]uintptr
		})(unsafe.Pointer(&buffer[0]))
		ok, _, err := queryJob.Call(job, 3, uintptr(unsafe.Pointer(info)), uintptr(len(buffer)*word), 0)
		if ok == 0 {
			if errors.Is(err, syscall.ERROR_MORE_DATA) {
				capacity = info.Assigned + 16
				continue
			}
			return fmt.Errorf("list process job members: %w", err)
		}
		for _, id := range unsafe.Slice(&info.PIDs[0], int(info.Count)) {
			pid := uint32(id)
			if _, exists := processes[pid]; exists {
				continue
			}
			const synchronize = 0x00100000
			process, err := syscall.OpenProcess(synchronize, false, pid)
			if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: exited
				continue
			}
			if err != nil {
				return fmt.Errorf("open job process %d for termination wait: %w", pid, err)
			}
			processes[pid] = process
		}
		return nil
	}
	return fmt.Errorf("process job enumeration exceeded termination deadline")
}
