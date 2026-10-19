package main

// ExecuteRequest is the payload sent from Next.js
type ExecuteRequest struct {
	Language       string `json:"language"`        // "cpp" or "java"
	Code           string `json:"code"`            // Source code
	Stdin          string `json:"stdin"`           // Standard input data
	TimeLimitMs    int    `json:"time_limit_ms"`   // e.g. 2000 (defaults to 2000ms if 0)
	MemoryLimitMB  int    `json:"memory_limit_mb"` // e.g. 64 (defaults to 64MB if 0)
}

// ExecuteResponse is returned back to Next.js
type ExecuteResponse struct {
	Status        string `json:"status"`         // "success", "compile_error", "timeout", "memory_limit", "runtime_error"
	CompileOutput string `json:"compile_output"` // Errors or warnings from g++ or javac
	Stdout        string `json:"stdout"`         // Program's standard output
	Stderr        string `json:"stderr"`         // Program's standard error
	DurationMs    int64  `json:"duration_ms"`    // Wall-clock runtime in milliseconds
	MemoryBytes   int64  `json:"memory_bytes"`   // Peak memory used in bytes (from isolate metadata)
	ExitCode      int    `json:"exit_code"`      // Process exit code (0 = success)
}

// BoxStats holds resource usage recorded by the Linux isolate sandbox
type BoxStats struct {
	TimeSec    float64 // CPU time in seconds
	WallSec    float64 // Wall-clock time in seconds
	MemoryKB   int64   // Memory consumed in KB
	Status     string  // "OK", "TO" (Time Out), "SG" (Signal/Crash), "RE" (Runtime Error)
	ExitCode   int     // Subprocess exit code
	Killed     bool    // Whether isolate killed the process
}