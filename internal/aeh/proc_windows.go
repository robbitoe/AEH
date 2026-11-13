//go:build windows

package aeh

import (
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
	// os/exec closes CreateProcess's primary thread handle. A suspended process
	// has only that thread, so retrieve it using the documented Toolhelp APIs.
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
	for ok, _, callErr := threadFirst.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry))); ; ok, _, callErr = threadNext.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry))) {
		if ok == 0 {
			return fmt.Errorf("find primary thread of process %d: %w", pid, callErr)
		}
		if entry.OwnerProcessID != pid {
			continue
		}
		const threadSuspendResume = 0x0002
		thread, _, err := openThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
		if thread == 0 {
			return fmt.Errorf("open primary thread of process %d: %w", pid, err)
		}
		defer syscall.CloseHandle(syscall.Handle(thread))
		if previous, _, err := resumeThread.Call(thread); previous == 0xffffffff {
			return fmt.Errorf("resume process %d: %w", pid, err)
		}
		return nil
	}
}

func killJob(job uintptr) error {
	if ok, _, err := terminateJob.Call(job, 1); ok == 0 {
		return fmt.Errorf("terminate process job: %w", err)
	}
	// Termination is asynchronous. Wait for every descendant to release its
	// handles before returning to callers that close logs or remove worktrees.
	deadline := time.Now().Add(2 * time.Second)
	for {
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
