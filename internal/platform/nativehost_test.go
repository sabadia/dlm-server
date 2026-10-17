package platform

import (
	"path/filepath"
	"testing"
)

func TestNativeHostRegistration(t *testing.T) {
	const name = "dev.test.host"
	home := filepath.Join("home", "u")
	mac := filepath.Join(home, "Library", "Application Support")
	cfg := filepath.Join(home, ".config")
	data := filepath.Join("data", "dlm")
	tests := []struct {
		goos string
		b    Browser
		path string
		key  string
	}{
		{"darwin", BrowserChrome, filepath.Join(mac, "Google", "Chrome", "NativeMessagingHosts", name+".json"), ""},
		{"darwin", BrowserChromium, filepath.Join(mac, "Chromium", "NativeMessagingHosts", name+".json"), ""},
		{"darwin", BrowserEdge, filepath.Join(mac, "Microsoft Edge", "NativeMessagingHosts", name+".json"), ""},
		{"darwin", BrowserBrave, filepath.Join(mac, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts", name+".json"), ""},
		{"darwin", BrowserFirefox, filepath.Join(mac, "Mozilla", "NativeMessagingHosts", name+".json"), ""},
		{"linux", BrowserChrome, filepath.Join(cfg, "google-chrome", "NativeMessagingHosts", name+".json"), ""},
		{"linux", BrowserChromium, filepath.Join(cfg, "chromium", "NativeMessagingHosts", name+".json"), ""},
		{"linux", BrowserEdge, filepath.Join(cfg, "microsoft-edge", "NativeMessagingHosts", name+".json"), ""},
		{"linux", BrowserBrave, filepath.Join(cfg, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts", name+".json"), ""},
		{"linux", BrowserFirefox, filepath.Join(home, ".mozilla", "native-messaging-hosts", name+".json"), ""},
		{"windows", BrowserChrome, filepath.Join(data, "native-messaging", "chrome", name+".json"), `Software\Google\Chrome\NativeMessagingHosts\` + name},
		{"windows", BrowserChromium, filepath.Join(data, "native-messaging", "chromium", name+".json"), `Software\Chromium\NativeMessagingHosts\` + name},
		{"windows", BrowserEdge, filepath.Join(data, "native-messaging", "edge", name+".json"), `Software\Microsoft\Edge\NativeMessagingHosts\` + name},
		{"windows", BrowserBrave, filepath.Join(data, "native-messaging", "brave", name+".json"), `Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + name},
		{"windows", BrowserFirefox, filepath.Join(data, "native-messaging", "firefox", name+".json"), `Software\Mozilla\NativeMessagingHosts\` + name},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+string(tt.b), func(t *testing.T) {
			userCfg := cfg
			if tt.goos == "darwin" {
				userCfg = mac
			}
			got, err := nativeHostRegistration(tt.goos, tt.b, name, home, userCfg, data)
			if err != nil {
				t.Fatal(err)
			}
			if got.ManifestPath != tt.path || got.RegistryKey != tt.key {
				t.Errorf("registration = %+v, want path %q key %q", got, tt.path, tt.key)
			}
		})
	}
}

func TestNativeHostRegistrationErrors(t *testing.T) {
	if _, err := nativeHostRegistration("linux", "netscape", "x", "h", "c", "d"); err == nil {
		t.Error("unknown browser accepted")
	}
	if _, err := nativeHostRegistration("plan9", BrowserChrome, "x", "h", "c", "d"); err == nil {
		t.Error("unsupported OS accepted")
	}
}

func TestBrowsers(t *testing.T) {
	for _, b := range Browsers() {
		if !b.Valid() {
			t.Errorf("%s is not valid", b)
		}
	}
	if Browser("x").Valid() || !BrowserFirefox.Firefox() || BrowserChrome.Firefox() {
		t.Error("Valid/Firefox misbehave")
	}
}
