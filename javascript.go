package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func getNodePath() string {
	path, err := exec.LookPath("node")
	if path == "" {
		return "/usr/bin/node"
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		return realPath
	}
	return path
}

func RunJavaScript(ctx context.Context, isolate *IsolateRunner, slot int, req ExecuteRequest) (*ExecuteResponse, error) {
	timeLimitSec := 2.5
	if req.TimeLimitMs > 0 {
		timeLimitSec = float64(req.TimeLimitMs) / 1000.0
	}

	boxDir, err := isolate.InitBox(slot)
	if err != nil {
		return nil, fmt.Errorf("init box: %w", err)
	}
	defer isolate.CleanupBox(slot)

	scriptPath := filepath.Join(boxDir, "index.js")
	if err := os.WriteFile(scriptPath, []byte(req.Code), 0777); err != nil {
		return nil, fmt.Errorf("write script: %w", err)
	}

	stdinPath := filepath.Join(boxDir, "stdin.txt")
	_ = os.WriteFile(stdinPath, []byte(req.Stdin), 0777)

	_ = exec.Command("chmod", "-R", "777", boxDir).Run()

	nodeBin := getNodePath()
	nodeDir := filepath.Dir(nodeBin)
	metaFile := fmt.Sprintf("/tmp/isolate_meta_%d.txt", slot)

	opts := RunOptions{
		BoxID:        slot,
		Command:      nodeBin,
		Args:         []string{"--max-old-space-size=48", "/box/index.js"},
		TimeLimitSec: timeLimitSec,
		WallLimitSec: timeLimitSec + 2.0,
		MemoryKB:     0,    // Let V8 allocate its virtual address space cleanly
		FileSizeKB:   1024, // 1 MB disk quota
		MaxProcesses: 20,   // Node uses libuv threadpool threads
		StdinFile:    "stdin.txt",
		StdoutFile:   "stdout.txt",
		StderrFile:   "stderr.txt",
		MetaFile:     metaFile,
		ExtraDirs: []string{
			nodeDir,
			"/usr/lib",
			"/usr/local/lib",
			"/etc",
		},
	}

	start := time.Now()
	stats, err := isolate.Run(ctx, opts)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("run js: %w", err)
	}

	stdoutBytes, _ := os.ReadFile(filepath.Join(boxDir, "stdout.txt"))
	stderrBytes, _ := os.ReadFile(filepath.Join(boxDir, "stderr.txt"))
	stdout := string(stdoutBytes)
	stderr := string(stderrBytes)

	// Cap output to 256 KB
	const maxOutput = 256 * 1024
	if len(stdout) > maxOutput {
		stdout = stdout[:maxOutput] + "\n[Output limit exceeded: Truncated to 256 KB]"
	}
	if len(stderr) > maxOutput {
		stderr = stderr[:maxOutput] + "\n[Error output limit exceeded: Truncated to 256 KB]"
	}

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
	} else if (opts.MemoryKB > 0 && stats.MemoryKB >= opts.MemoryKB) || stats.Killed {
		status = "memory_limit"
		if stderr == "" {
			stderr = "Memory Limit Exceeded"
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
