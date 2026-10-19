package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// RunCpp handles the compilation and sandboxed execution of C++ code
func RunCpp(ctx context.Context, isolate *IsolateRunner, boxID int, req ExecuteRequest) (*ExecuteResponse, error) {
	// 1. Initialize clean sandbox box (creates /var/local/lib/isolate/{boxID}/box)
	boxDir, err := isolate.InitBox(boxID)
	if err != nil {
		return nil, fmt.Errorf("failed to init box: %v", err)
	}
	defer isolate.CleanupBox(boxID) // Wipe box and delete temp files when done

	// 2. Set default limits if not specified
	timeLimitMs := req.TimeLimitMs
	if timeLimitMs <= 0 {
		timeLimitMs = 2000 // default 2 seconds
	}
	memLimitMB := req.MemoryLimitMB
	if memLimitMB <= 0 {
		memLimitMB = 64 // default 64 MB
	}

	// 3. Write source code and stdin into the sandbox directory
	cppFile := filepath.Join(boxDir, "main.cpp")
	if err := os.WriteFile(cppFile, []byte(req.Code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write source: %v", err)
	}

	stdinFile := filepath.Join(boxDir, "stdin.txt")
	if err := os.WriteFile(stdinFile, []byte(req.Stdin), 0644); err != nil {
		return nil, fmt.Errorf("failed to write stdin: %v", err)
	}

	// 4. Compile Phase with Compiler DOS protections:
	// - 5s timeout: kills infinite macro/template loops
	// - -ftemplate-depth=128: prevents template recursion bomb
	// - -fmax-errors=3: stops compiler error spamming
	// - -O0 -pipe: ultra-fast compilation without disk churn
	compileCtx, compileCancel := context.WithTimeout(ctx, 5*time.Second)
	defer compileCancel()

	compileCmd := exec.CommandContext(compileCtx, "g++",
		"-O0",
		"-std=c++17",
		"-pipe",
		"-fmax-errors=3",
		"-ftemplate-depth=128",
		"main.cpp",
		"-o", "main",
	)
	compileCmd.Dir = boxDir

	compileOut, err := compileCmd.CombinedOutput()
	if err != nil {
		return &ExecuteResponse{
			Status:        "compile_error",
			CompileOutput: string(compileOut),
			ExitCode:      1,
		}, nil
	}

	// 5. Execution Phase inside isolate sandbox
	metaFile := filepath.Join(boxDir, "meta.txt")
	opts := RunOptions{
		BoxID:        boxID,
		Command:      "./main",
		TimeLimitSec: float64(timeLimitMs) / 1000.0,
		WallLimitSec: (float64(timeLimitMs) / 1000.0) * 2.0,
		MemoryKB:     int64(memLimitMB) * 1024,
		FileSizeKB:   1024, // 1 MB hard disk quota to prevent infinite output flooding
		MaxProcesses: 10,   // Headroom for glibc threads & fast sequential executions, blocks fork bombs
		StdinFile:    "stdin.txt",
		StdoutFile:   "stdout.txt",
		StderrFile:   "stderr.txt",
		MetaFile:     metaFile,
	}

	startTime := time.Now()
	stats, err := isolate.Run(ctx, opts)
	durationMs := time.Since(startTime).Milliseconds()

	if err != nil {
		return nil, fmt.Errorf("isolate execution error: %v", err)
	}

	// 6. Read stdout and stderr from sandbox
	stdoutBytes, _ := os.ReadFile(filepath.Join(boxDir, "stdout.txt"))
	stderrBytes, _ := os.ReadFile(filepath.Join(boxDir, "stderr.txt"))

	stdout := string(stdoutBytes)
	stderr := string(stderrBytes)

	// Cap output to 256 KB to prevent JSON serialization bloat
	const maxOutput = 256 * 1024
	if len(stdout) > maxOutput {
		stdout = stdout[:maxOutput] + "\n[Output limit exceeded: Truncated to 256 KB]"
	}
	if len(stderr) > maxOutput {
		stderr = stderr[:maxOutput] + "\n[Error output limit exceeded: Truncated to 256 KB]"
	}

	// 7. Determine final status from isolate metadata
	status := "success"
	if stats.Status == "TO" {
		status = "timeout"
		if stderr == "" {
			stderr = fmt.Sprintf("Time Limit Exceeded: Execution took longer than %.1fs", opts.TimeLimitSec)
		}
	} else if stats.Status == "FO" {
		status = "output_limit"
		if stderr == "" {
			stderr = "Output Limit Exceeded: Program exceeded 1 MB output quota"
		}
	} else if stats.MemoryKB >= opts.MemoryKB || stats.Killed {
		status = "memory_limit"
		if stderr == "" {
			stderr = fmt.Sprintf("Memory Limit Exceeded: Exceeded %d MB cap", memLimitMB)
		}
	} else if stats.ExitCode != 0 || stats.Status == "RE" || stats.Status == "SG" || stats.Status == "XX" {
		status = "runtime_error"
	}

	return &ExecuteResponse{
		Status:      status,
		Stdout:      stdout,
		Stderr:      stderr,
		DurationMs:  durationMs,
		MemoryBytes: stats.MemoryKB * 1024,
		ExitCode:    stats.ExitCode,
	}, nil
}
