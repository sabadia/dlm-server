package postproc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// The test binary doubles as the program the actions run: with
// DLM_TEST_PROG set it acts out the command line instead of running tests.
//
//	exit N [text]   print text, exit with status N
//	record FILE ... write the remaining arguments, one per line, to FILE
//	sleep           block for a minute
func TestMain(m *testing.M) {
	if os.Getenv("DLM_TEST_PROG") == "1" {
		os.Exit(fakeProgram(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeProgram(args []string) int {
	if len(args) == 0 {
		return 99
	}
	switch args[0] {
	case "exit":
		n, _ := strconv.Atoi(args[1])
		if len(args) > 2 {
			fmt.Println(strings.Join(args[2:], " "))
		}
		return n
	case "record":
		if err := os.WriteFile(args[1], []byte(strings.Join(args[2:], "\n")), 0o600); err != nil {
			return 98
		}
		return 0
	case "sleep":
		time.Sleep(time.Minute)
	}
	return 0
}

func testProgram(t *testing.T) string {
	t.Helper()
	t.Setenv("DLM_TEST_PROG", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func argsJSON(args ...string) string {
	out := "["
	for i, a := range args {
		if i > 0 {
			out += ","
		}
		out += strconv.Quote(a)
	}
	return out + "]"
}

func programJob(t *testing.T) *Job {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Job{Task: domain.Task{ID: "task1", Dir: dir, Name: "file.bin", Size: 4, Source: domain.Source{CurrentURL: "http://x/f"}}, Path: path}
}

func TestCommandActionExpandsTemplatesAndRunsWithoutAShell(t *testing.T) {
	prog := testProgram(t)
	j := programJob(t)
	out := filepath.Join(t.TempDir(), "args.txt")
	// A shell would split or expand these; a direct exec passes them verbatim.
	params := map[string]string{"cmd": prog, "args": argsJSON("record", out, "{path}", "{dir}", "{name}", "{taskId}", "{url}", "{size}", "a b;$HOME")}
	if err := (Command{}).Run(t.Context(), j, params); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{j.Path, j.Task.Dir, "file.bin", "task1", "http://x/f", "4", "a b;$HOME"}, "\n")
	if string(got) != want {
		t.Errorf("args = %q, want %q", got, want)
	}
	if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "ok") {
		t.Errorf("notes = %v", j.Notes)
	}
}

func TestCommandActionDefaultsToThePathAndReportsFailures(t *testing.T) {
	prog := testProgram(t)
	j := programJob(t)
	// With no args the path is the only argument: "exit" gets a non-number
	// there, so give it args explicitly for the failure cases.
	err := Command{}.Run(t.Context(), j, map[string]string{"cmd": prog, "args": argsJSON("exit", "3", "it", "broke")})
	if err == nil || !strings.Contains(err.Error(), "status 3") || !strings.Contains(err.Error(), "it broke") {
		t.Errorf("non-zero exit: %v", err)
	}
	if err := (Command{}).Run(t.Context(), j, map[string]string{"cmd": "relative/prog"}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative cmd: %v", err)
	}
	if err := (Command{}).Run(t.Context(), j, map[string]string{"cmd": filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("a missing program did not fail")
	}
	if err := (Command{}).Run(t.Context(), j, map[string]string{"cmd": prog, "args": "not json"}); err == nil || !strings.Contains(err.Error(), "JSON array") {
		t.Errorf("bad args: %v", err)
	}
	if err := (Command{}).Run(t.Context(), j, map[string]string{"cmd": prog, "args": argsJSON("sleep"), "timeout": "100ms"}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
	if err := (Command{}).Run(t.Context(), j, map[string]string{"cmd": prog, "timeout": "soon"}); err == nil {
		t.Error("bad timeout accepted")
	}
}

func TestAVScanActionOutcomes(t *testing.T) {
	prog := testProgram(t)
	scan := func(t *testing.T, j *Job, params map[string]string) error {
		t.Helper()
		params["cmd"] = prog
		return AVScan{}.Run(t.Context(), j, params)
	}

	t.Run("clean", func(t *testing.T) {
		j := programJob(t)
		if err := scan(t, j, map[string]string{"args": argsJSON("exit", "0")}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(j.Path); err != nil {
			t.Error("a clean file was removed")
		}
		if len(j.Notes) != 1 || !strings.Contains(j.Notes[0], "clean") {
			t.Errorf("notes = %v", j.Notes)
		}
	})
	t.Run("infected is deleted", func(t *testing.T) {
		j := programJob(t)
		err := scan(t, j, map[string]string{"args": argsJSON("exit", "1", "Eicar FOUND")})
		var inf *InfectedError
		if !errors.As(err, &inf) || !inf.Deleted || !strings.Contains(err.Error(), "Eicar FOUND") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(j.Path); !os.IsNotExist(err) {
			t.Errorf("the infected file is still there: %v", err)
		}
	})
	t.Run("infected is kept on request, with another status", func(t *testing.T) {
		j := programJob(t)
		err := scan(t, j, map[string]string{"args": argsJSON("exit", "7"), "infectedExit": "7", "onInfected": "keep"})
		var inf *InfectedError
		if !errors.As(err, &inf) || inf.Deleted {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(j.Path); err != nil {
			t.Error("the file was removed despite onInfected=keep")
		}
	})
	t.Run("a scanner failure is not an infection", func(t *testing.T) {
		j := programJob(t)
		err := scan(t, j, map[string]string{"args": argsJSON("exit", "2", "database missing")})
		var inf *InfectedError
		if err == nil || errors.As(err, &inf) || !strings.Contains(err.Error(), "status 2") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(j.Path); err != nil {
			t.Error("the file was removed after a scanner failure")
		}
	})
	t.Run("bad infectedExit", func(t *testing.T) {
		if err := scan(t, programJob(t), map[string]string{"infectedExit": "x"}); err == nil {
			t.Error("accepted")
		}
	})
}

func TestActionsStepFailsOnAnInfectedFile(t *testing.T) {
	prog := testProgram(t)
	j := programJob(t)
	j.Settings.PostActions = settings.Some([]settings.PostAction{
		{Kind: settings.PostActionAVScan, Params: map[string]string{"cmd": prog, "args": argsJSON("exit", "1")}},
		{Kind: settings.PostActionCommand, Params: map[string]string{"cmd": prog, "args": argsJSON("exit", "0")}},
	})
	a := NewActions(nil, map[string]Action{settings.PostActionAVScan: AVScan{}, settings.PostActionCommand: Command{}})
	var inf *InfectedError
	if err := a.Run(t.Context(), j); !errors.As(err, &inf) {
		t.Fatalf("err = %v, want an *InfectedError", err)
	}
	if len(j.Notes) != 0 {
		t.Errorf("the action after the infection ran: %v", j.Notes)
	}
}
