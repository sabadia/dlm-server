package postproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultCommandTimeout bounds a command that has no timeout parameter.
const DefaultCommandTimeout = 5 * time.Minute

// maxCommandOutput is how much of a command's output is kept for the notes.
const maxCommandOutput = 400

// InfectedError reports a file the virus scanner flagged; the file has been
// handled as the action was configured (deleted unless onInfected is keep) and
// the task must not complete.
type InfectedError struct {
	Path    string
	Output  string
	Deleted bool
}

// Error implements error.
func (e *InfectedError) Error() string {
	msg := "virus scan flagged " + filepath.Base(e.Path)
	if e.Deleted {
		msg += " (deleted)"
	}
	if e.Output != "" {
		msg += ": " + e.Output
	}
	return msg
}

// Command is the post action that runs a program on the downloaded file
// (parameters: cmd, args, timeout). The program is started directly, never
// through a shell; args is a JSON array of strings in which {path}, {dir},
// {name}, {taskId}, {url} and {size} are replaced.
type Command struct{}

// Run implements Action.
func (Command) Run(ctx context.Context, j *Job, params map[string]string) error {
	res, err := runProgram(ctx, j, params)
	if err != nil {
		return err
	}
	if res.exit != 0 {
		return fmt.Errorf("%s exited with status %d%s", filepath.Base(params["cmd"]), res.exit, res.outputSuffix())
	}
	j.Note("command %s: ok", filepath.Base(params["cmd"]))
	return nil
}

// AVScan is the post action that scans the downloaded file with an external
// virus scanner (parameters: cmd, args, timeout, infectedExit, onInfected).
// The scanner's exit status infectedExit (default 1, ClamAV's) means infected:
// the file is deleted (onInfected "keep" leaves it) and the task fails with an
// *InfectedError. Any other non-zero status means the scan itself failed, which
// is only a warning.
type AVScan struct{}

// Run implements Action.
func (AVScan) Run(ctx context.Context, j *Job, params map[string]string) error {
	infectedExit := 1
	if v := params["infectedExit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("infectedExit %q is not a number", v)
		}
		infectedExit = n
	}
	res, err := runProgram(ctx, j, params)
	if err != nil {
		return err
	}
	name := filepath.Base(params["cmd"])
	switch res.exit {
	case 0:
		j.Note("av-scan %s: clean", name)
		return nil
	case infectedExit:
		inf := &InfectedError{Path: j.Path, Output: res.output}
		if params["onInfected"] != "keep" {
			if err := os.RemoveAll(j.Path); err != nil {
				return fmt.Errorf("remove infected file: %w", errors.Join(inf, err))
			}
			inf.Deleted = true
		}
		return inf
	default:
		return fmt.Errorf("scanner %s failed with status %d%s", name, res.exit, res.outputSuffix())
	}
}

type programResult struct {
	exit   int
	output string
}

func (r programResult) outputSuffix() string {
	if r.output == "" {
		return ""
	}
	return ": " + r.output
}

// runProgram starts the configured program for the job and waits for it. It
// returns an error when the program cannot run or times out; a program that
// ran and exited is reported through the result.
func runProgram(ctx context.Context, j *Job, params map[string]string) (programResult, error) {
	prog := params["cmd"]
	if !filepath.IsAbs(prog) {
		return programResult{}, errors.New("cmd must be an absolute path")
	}
	args, err := expandArgs(j, params["args"])
	if err != nil {
		return programResult{}, err
	}
	timeout := DefaultCommandTimeout
	if v := params["timeout"]; v != "" {
		if timeout, err = time.ParseDuration(v); err != nil || timeout <= 0 {
			return programResult{}, fmt.Errorf("timeout %q is not a positive duration", v)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(runCtx, prog, args...) //nolint:gosec // running the configured program is the point of the action
	cmd.Dir = j.Task.Dir
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return programResult{output: tail(out.String())}, nil
	case runCtx.Err() != nil && ctx.Err() == nil:
		return programResult{}, fmt.Errorf("%s timed out after %s", filepath.Base(prog), timeout)
	case ctx.Err() != nil:
		return programResult{}, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	case errors.As(err, &exitErr):
		return programResult{exit: exitErr.ExitCode(), output: tail(out.String())}, nil
	default:
		return programResult{}, fmt.Errorf("start %s: %w", filepath.Base(prog), err)
	}
}

// expandArgs decodes the JSON array of arguments and fills in the job's
// values. With no args the file path is the only argument.
func expandArgs(j *Job, raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{j.Path}, nil
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("args must be a JSON array of strings: %w", err)
	}
	r := strings.NewReplacer(
		"{path}", j.Path,
		"{dir}", filepath.Dir(j.Path),
		"{name}", filepath.Base(j.Path),
		"{taskId}", j.Task.ID,
		"{url}", j.Task.Source.CurrentURL,
		"{size}", strconv.FormatInt(j.Task.Size, 10),
	)
	for i, a := range args {
		args[i] = r.Replace(a)
	}
	return args, nil
}

// tail keeps the last part of a program's output on one line.
func tail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxCommandOutput {
		s = "…" + s[len(s)-maxCommandOutput:]
	}
	return s
}
