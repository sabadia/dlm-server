package ytdlp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"testing"
	"time"
)

// The tests run the test binary itself as yt-dlp: with DLM_FAKE_YTDLP=1 it
// acts as a yt-dlp whose behaviour the FAKE_* variables choose, so no real
// yt-dlp is needed.

const defaultJSON = `{"id":"abc123","title":"My Video: Part 1","ext":"mp4","_type":"video",
 "requested_formats":[{"filesize":3000},{"filesize_approx":1000.5}]}`

func TestMain(m *testing.M) {
	if os.Getenv("DLM_FAKE_YTDLP") == "1" {
		fakeYtdlp()
		return
	}
	os.Exit(m.Run())
}

func fakeYtdlp() {
	args := os.Args[1:]
	if log := os.Getenv("FAKE_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			rec := map[string]any{"args": args}
			// Record what the cookies file held while the process ran.
			for _, a := range args {
				if p, ok := strings.CutPrefix(a, "--cookies="); ok {
					if b, err := os.ReadFile(p); err == nil {
						rec["cookies"] = string(b)
					}
					if fi, err := os.Stat(p); err == nil {
						rec["cookiesMode"] = fi.Mode().Perm().String()
					}
				}
			}
			_ = json.NewEncoder(f).Encode(rec)
			_ = f.Close()
		}
	}
	has := func(a string) bool {
		for _, x := range args {
			if x == a {
				return true
			}
		}
		return false
	}
	mode := os.Getenv("FAKE_MODE")
	fail := func(msg string) {
		fmt.Fprintln(os.Stderr, "ERROR: "+msg)
		os.Exit(1)
	}
	if has("--dump-single-json") {
		if mode == "resolve-fail" {
			fail(os.Getenv("FAKE_ERROR"))
		}
		js := os.Getenv("FAKE_JSON")
		if js == "" {
			js = defaultJSON
		}
		fmt.Println(js)
		return
	}
	if mode == "fail" {
		fail(os.Getenv("FAKE_ERROR"))
	}
	var out string
	for _, a := range args {
		if o, ok := strings.CutPrefix(a, "--output="); ok {
			out = strings.ReplaceAll(o, "%(ext)s", "mp4")
		}
	}
	body := os.Getenv("FAKE_BODY")
	switch mode {
	case "slow":
		fmt.Println("DLM|downloading|1000|4000|NA|" + out)
		_ = os.WriteFile(out+".part", []byte("partial"), 0o600)
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		select {
		case <-c:
			os.Exit(130)
		case <-time.After(30 * time.Second):
			os.Exit(1)
		}
	case "merge":
		fmt.Println("DLM|downloading|500|1000|NA|v.f1.mp4")
		fmt.Println("DLM|finished|1000|1000|NA|v.f1.mp4")
		fmt.Println("DLM|downloading|200|NA|400.5|a.f2.m4a")
		fmt.Println("DLM|finished|400|NA|NA|a.f2.m4a")
	default:
		fmt.Printf("DLM|downloading|%d|%d|NA|%s\n", len(body)/2, len(body), out)
		fmt.Printf("DLM|finished|%d|%d|NA|%s\n", len(body), len(body), out)
	}
	fmt.Println("some other line")
	switch mode {
	case "outside":
		out = os.Getenv("FAKE_OUTSIDE")
	case "nofile":
		return
	}
	if err := os.WriteFile(out, []byte(body), 0o600); err != nil {
		fail(err.Error())
	}
	fmt.Println("DLMFILE|" + out)
}
