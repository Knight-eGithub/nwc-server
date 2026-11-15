package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// getJavaPaths resolves the real Java binary, javac binary, and JDK home directory (even through symlinks/SDKMAN)
func getJavaPaths() (realJava string, realJavac string, jdkHome string) {
	javaBin, err := exec.LookPath("java")
	if err != nil {
		javaBin = "/usr/bin/java"
	}
	realJava, err = filepath.EvalSymlinks(javaBin)
	if err != nil {
		realJava = javaBin
	}

	javacBin, err := exec.LookPath("javac")
	if err != nil {
		javacBin = "/usr/bin/javac"
	}
	realJavac, err = filepath.EvalSymlinks(javacBin)
	if err != nil {
		realJavac = javacBin
	}

	// JDK Home is two directories up from .../bin/java
	jdkHome = filepath.Dir(filepath.Dir(realJava))
	if _, err := os.Stat(filepath.Join(jdkHome, "bin", "javac")); err != nil {
		altHome := filepath.Dir(filepath.Dir(realJavac))
		if _, err := os.Stat(filepath.Join(altHome, "bin", "javac")); err == nil {
			jdkHome = altHome
		}
	}
	return realJava, realJavac, jdkHome
}

// RunJava handles the compilation and sandboxed execution of Java code
func RunJava(ctx context.Context, isolate *IsolateRunner, boxID int, req ExecuteRequest) (*ExecuteResponse, error) {
	// 1. Initialize clean sandbox box
	boxDir, err := isolate.InitBox(boxID)
	if err != nil {
		return nil, fmt.Errorf("failed to init box: %v", err)
	}
	defer isolate.CleanupBox(boxID)

	// 2. Default limits
	timeLimitMs := req.TimeLimitMs
	if timeLimitMs <= 0 {
		timeLimitMs = 2500 // 2.5s default for Java
	}

	// 3. Write Main.java and stdin.txt into sandbox
	javaFile := filepath.Join(boxDir, "Main.java")
	if err := os.WriteFile(javaFile, []byte(req.Code), 0644); err != nil {
		return nil, fmt.Errorf("failed to write Java source: %v", err)
	}

	stdinFile := filepath.Join(boxDir, "stdin.txt")
	if err := os.WriteFile(stdinFile, []byte(req.Stdin), 0644); err != nil {
		return nil, fmt.Errorf("failed to write stdin: %v", err)
	}

	// 4. Compilation Phase on host with guards:
	// - 8s timeout
	// - -Xmaxerrs 3: stops compilation after 3 errors
	compileCtx, compileCancel := context.WithTimeout(ctx, 8*time.Second)
	defer compileCancel()

	realJavaBin, realJavacBin, jdkHome := getJavaPaths()

	compileCmd := exec.CommandContext(compileCtx, realJavacBin,
		"-J-Xmx128m",
		"-J-Xms16m",
		"-Xmaxerrs", "3",
		"Main.java",
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
	_ = exec.Command("chmod", "-R", "777", boxDir).Run()

	// 5. Detect real Java binary and JDK installation directory
	metaFile := fmt.Sprintf("/tmp/isolate_meta_%d.txt", boxID)

	// JVM runtime flags strictly tailored for low-memory CP execution
	javaArgs := []string{
		"-Xmx128m",
		"-Xms16m",
		"-XX:MaxMetaspaceSize=64m",
		"-Xshare:off",
		"-XX:ActiveProcessorCount=2",
		"-XX:CICompilerCount=2",
		"-XX:+UseSerialGC",
		"-XX:+TieredCompilation",
		"-XX:TieredStopAtLevel=1",
		"-Xss512k",
		"-cp", "/box",
		"Main",
	}

	// Mount the JDK directory and system configs into the sandbox (read-only by default in isolate)
	extraDirs := []string{
		jdkHome,
		"/etc",
	}

	opts := RunOptions{
		BoxID:        boxID,
		Command:      realJavaBin,
		Args:         javaArgs,
		TimeLimitSec: float64(timeLimitMs) / 1000.0,
		WallLimitSec: (float64(timeLimitMs) / 1000.0) * 2.0,
		MemoryKB:     0,    // 0 lets JVM manage its virtual address space without triggering SIGSEGV 11
		FileSizeKB:   1024, // 1 MB disk quota
		MaxProcesses: 30,   // Capped to 30 threads for JVM GC/JIT, blocks thread bombs
		ExtraDirs:    extraDirs,
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

	// Cap output to 256 KB
	const maxOutput = 256 * 1024
	if len(stdout) > maxOutput {
		stdout = stdout[:maxOutput] + "\n[Output limit exceeded: Truncated to 256 KB]"
	}
	if len(stderr) > maxOutput {
		stderr = stderr[:maxOutput] + "\n[Error output limit exceeded: Truncated to 256 KB]"
	}

	// 7. Determine final status
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
			stderr = "Memory Limit Exceeded: Java process exceeded memory cap"
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
