package platform

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	calls []string
	fail  map[string]error
}

func (r *recorder) run(_ context.Context, c Command) error {
	r.calls = append(r.calls, c.String())
	for prefix, err := range r.fail {
		if strings.HasPrefix(c.String(), prefix) {
			return err
		}
	}
	return nil
}

type fakeRegistry struct{ values map[string]string }

func (f *fakeRegistry) Set(name, command string) error { f.values[name] = command; return nil }
func (f *fakeRegistry) Delete(name string) error       { delete(f.values, name); return nil }
func (f *fakeRegistry) Get(name string) (string, bool, error) {
	v, ok := f.values[name]
	return v, ok, nil
}

var spec = AutostartSpec{Executable: "/opt/my apps/dlm", Args: []string{"serve", "--home", "/data/d&d"}}

func TestLaunchdPlistIsValidXMLWithEscapedArguments(t *testing.T) {
	plist := LaunchdPlist(spec)
	var doc struct {
		Dict struct {
			Keys    []string `xml:"key"`
			Strings []string `xml:"string"`
			Array   struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(plist), &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, plist)
	}
	if got := strings.Join(doc.Dict.Array.Strings, "|"); got != "/opt/my apps/dlm|serve|--home|/data/d&d" {
		t.Errorf("arguments = %q", got)
	}
	if !strings.Contains(plist, AutostartLabel) || !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Errorf("plist:\n%s", plist)
	}
}

func TestSystemdUnitQuotesWhatNeedsIt(t *testing.T) {
	unit := SystemdUnit(AutostartSpec{Executable: "/opt/my apps/dlm", Args: []string{"serve", "--name=100%", `a"b`}})
	want := `ExecStart="/opt/my apps/dlm" serve "--name=100%%" "a\"b"`
	if !strings.Contains(unit, want) {
		t.Errorf("unit:\n%s\nwant a line %s", unit, want)
	}
	for _, line := range []string{"[Unit]", "[Service]", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(unit, line) {
			t.Errorf("unit lacks %q", line)
		}
	}
	if got := systemdQuote("plain-word"); got != "plain-word" {
		t.Errorf("a plain word was quoted: %q", got)
	}
}

func TestWindowsRunCommand(t *testing.T) {
	got := WindowsRunCommand(AutostartSpec{Executable: `C:\Program Files\dlm\dlm.exe`, Args: []string{"serve"}})
	if got != `"C:\Program Files\dlm\dlm.exe" serve` {
		t.Errorf("command = %s", got)
	}
}

func TestLaunchdInstallStatusUninstall(t *testing.T) {
	home := t.TempDir()
	rec := &recorder{}
	a := &Autostart{GOOS: "darwin", Home: home, UID: 501, Run: rec.run}

	if st, err := a.Status(t.Context()); err != nil || st.Installed {
		t.Fatalf("before installing: %+v, %v", st, err)
	}
	st, err := a.Install(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", AutostartLabel+".plist")
	if st.Kind != "launchd" || !st.Installed || !st.Active || st.Path != plist {
		t.Errorf("installed = %+v", st)
	}
	if data, err := os.ReadFile(plist); err != nil || !strings.Contains(string(data), "/opt/my apps/dlm") {
		t.Errorf("plist = %q, %v", data, err)
	}
	if got := strings.Join(rec.calls, "; "); got != "launchctl bootout gui/501/"+AutostartLabel+"; launchctl bootstrap gui/501 "+plist {
		t.Errorf("commands = %s", got)
	}
	if st, _ := a.Status(t.Context()); !st.Installed || !st.Active {
		t.Errorf("status = %+v", st)
	}
	rec.fail = map[string]error{"launchctl print": errors.New("not loaded")}
	if st, _ := a.Status(t.Context()); !st.Installed || st.Active {
		t.Errorf("installed but not loaded: %+v", st)
	}

	if err := a.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plist is still there (%v)", err)
	}
	if err := a.Uninstall(t.Context()); err != nil {
		t.Errorf("uninstalling twice: %v", err)
	}
}

func TestSystemdInstallStatusUninstall(t *testing.T) {
	cfg := t.TempDir()
	rec := &recorder{}
	a := &Autostart{GOOS: "linux", Home: t.TempDir(), UserConfigDir: cfg, Run: rec.run}
	st, err := a.Install(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(cfg, "systemd", "user", "dlm.service")
	if st.Kind != "systemd" || !st.Installed || !st.Active || st.Path != unit {
		t.Errorf("installed = %+v", st)
	}
	if got := strings.Join(rec.calls, "; "); got != "systemctl --user daemon-reload; systemctl --user enable --now dlm.service" {
		t.Errorf("commands = %s", got)
	}
	rec.calls = nil
	if err := a.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.calls, "; "); got != "systemctl --user disable --now dlm.service; systemctl --user daemon-reload" {
		t.Errorf("uninstall commands = %s", got)
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Error("the unit is still there")
	}

	// A failing systemctl is reported, with what was written noted.
	bad := &Autostart{GOOS: "linux", Home: t.TempDir(), UserConfigDir: t.TempDir(), Run: (&recorder{fail: map[string]error{"systemctl --user enable": errors.New("no bus")}}).run}
	st, err = bad.Install(t.Context(), spec)
	if err == nil || !st.Installed || st.Active {
		t.Errorf("failing enable: %+v, %v", st, err)
	}
}

func TestWindowsInstallStatusUninstall(t *testing.T) {
	reg := &fakeRegistry{values: map[string]string{}}
	a := &Autostart{GOOS: "windows", Registry: reg}
	spec := AutostartSpec{Executable: `C:\dlm\dlm.exe`, Args: []string{"serve"}}
	if _, err := a.Install(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if reg.values["dlm"] != `C:\dlm\dlm.exe serve` {
		t.Errorf("registry = %v", reg.values)
	}
	if st, _ := a.Status(t.Context()); !st.Installed || st.Kind != "registry" {
		t.Errorf("status = %+v", st)
	}
	if err := a.Uninstall(t.Context()); err != nil || len(reg.values) != 0 {
		t.Errorf("uninstall: %v, %v", err, reg.values)
	}
}

func TestAutostartRefusals(t *testing.T) {
	a := &Autostart{GOOS: "plan9"}
	if _, err := a.Install(t.Context(), spec); !errors.Is(err, ErrAutostartUnsupported) {
		t.Errorf("unsupported system: %v", err)
	}
	if _, err := (&Autostart{GOOS: "linux"}).Install(t.Context(), AutostartSpec{Executable: "relative/dlm"}); err == nil {
		t.Error("a relative executable was accepted")
	}
}
