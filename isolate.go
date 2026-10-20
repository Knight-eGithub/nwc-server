package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type IsolateRunner struct {
	BaseDir string
}

func NewIsolateRunner() *IsolateRunner {
	return &IsolateRunner{
		BaseDir: "/var/local/lib/isolate",
	}
}

func (r *IsolateRunner) InitBox(boxID int) (string, error) {
	_ = r.CleanupBox(boxID) // ensure any stale box or processes are purged before initializing
	cmd := exec.Command("isolate", fmt.Sprintf("--box-id=%d", boxID), "--init")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("isolate init failed: %v, output: %s", err, string(out))
	}
	boxPath := strings.TrimSpace(string(out))
	if !strings.HasSuffix(boxPath, "box") {
		boxPath = filepath.Join(boxPath, "box")
	}
	return boxPath, nil
}

func (r *IsolateRunner) CleanupBox(boxID int) error {
	// 1. Kill any stuck isolate supervisor processes for this box
	_ = exec.Command("pkill", "-9", "-f", fmt.Sprintf("isolate --box-id=%d", boxID)).Run()

	// 2. Kill all lingering processes owned by this box's sandbox UID (Codespaces uses 100000+boxID, standard uses 60000+boxID)
	for _, baseUID := range []int{100000, 60000} {
		uid := baseUID + boxID
		_ = exec.Command("pkill", "-9", "-u", strconv.Itoa(uid)).Run()
	}

	// 3. Clean up the box directory and unmount filesystems
	cmd := exec.Command("isolate", fmt.Sprintf("--box-id=%d", boxID), "--cleanup")
	return cmd.Run()
}

type RunOptions struct {
	BoxID        int
	Command      string   // Executable name or path inside box
	Args         []string // Program arguments
	TimeLimitSec float64  // e.g. 2.0 seconds CPU time
	WallLimitSec float64  // e.g. 4.0 seconds Wall clock time
	MemoryKB     int64    // e.g. 65536 KB (64 MB)
	FileSizeKB   int64    // e.g. 1024 KB (1 MB disk quota)
	MaxProcesses int      // 1 for C++, 30 for Java (JVM background threads)
	ExtraDirs    []string // Additional directories to mount inside sandbox
	StdinFile    string   // Filename inside box (e.g. "stdin.txt")
	StdoutFile   string   // Filename inside box (e.g. "stdout.txt")
	StderrFile   string   // Filename inside box (e.g. "stderr.txt")
	MetaFile     string   // Absolute path to metadata output file
}

func (r *IsolateRunner) Run(ctx context.Context, opts RunOptions) (*BoxStats, error) {
	fsize := opts.FileSizeKB
	if fsize <= 0 {
		fsize = 1024 // 1 MB default hard disk quota
	}

	args := []string{
		fmt.Sprintf("--box-id=%d", opts.BoxID),
		fmt.Sprintf("--meta=%s", opts.MetaFile),
		fmt.Sprintf("--time=%.3f", opts.TimeLimitSec),
		fmt.Sprintf("--wall-time=%.3f", opts.WallLimitSec),
		fmt.Sprintf("--fsize=%d", fsize),
		fmt.Sprintf("--processes=%d", opts.MaxProcesses),
		"--chdir=/box",
		"--silent",
	}

	// Only apply RLIMIT_AS memory limit if explicitly specified (> 0)
	if opts.MemoryKB > 0 {
		args = append(args, fmt.Sprintf("--mem=%d", opts.MemoryKB))
	}

	for _, dir := range opts.ExtraDirs {
		args = append(args, fmt.Sprintf("--dir=%s", dir))
	}

	if opts.StdinFile != "" {
		args = append(args, fmt.Sprintf("--stdin=%s", opts.StdinFile))
	}

	if opts.StdoutFile != "" {
		args = append(args, fmt.Sprintf("--stdout=%s", opts.StdoutFile))
	}

	if opts.StderrFile != "" {
		args = append(args, fmt.Sprintf("--stderr=%s", opts.StderrFile))
	}

	args = append(args, "--run", "--", opts.Command)
	args = append(args, opts.Args...)

	cmd := exec.CommandContext(ctx, "isolate", args...)
	_ = cmd.Run()

	stats, err := r.ParseMetaFile(opts.MetaFile)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %v", err)
	}

	return stats, nil
}

func (r *IsolateRunner) ParseMetaFile(path string) (*BoxStats, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stats := &BoxStats{
		Status: "OK",
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "time":
			stats.TimeSec, _ = strconv.ParseFloat(val, 64)
		case "time-wall":
			stats.WallSec, _ = strconv.ParseFloat(val, 64)
		case "max-rss":
			stats.MemoryKB, _ = strconv.ParseInt(val, 10, 64)
		case "status":
			stats.Status = val // "TO" = Timeout, "FO" = File size exceeded, "SG" = Signal, "RE" = Runtime Error, "XX" = Internal error
		case "exitcode":
			stats.ExitCode, _ = strconv.Atoi(val)
		case "killed":
			stats.Killed = (val == "1")
		}
	}

	return stats, nil
}

func (r *IsolateRunner) GetBoxDir(boxID int) string {
	return filepath.Join(r.BaseDir, strconv.Itoa(boxID), "box")
}
