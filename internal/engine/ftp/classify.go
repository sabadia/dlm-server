package ftp

import (
	"errors"
	"fmt"
	"net/textproto"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
)

// classify turns an FTP or network error into an *engine.Error.
//
//	421, 425, 426, 4xx             → transient (network)
//	530 (login failed)             → link_expired: the credentials in the link are no longer valid
//	550 (file unavailable)         → link_expired: the file is gone or not readable
//	other 5xx                      → fatal
//	other errors                   → httpx.Classify (network errors transient, TLS and disk fatal)
func classify(err error) error {
	if err == nil {
		return nil
	}
	var ee *engine.Error
	if errors.As(err, &ee) {
		return err
	}
	var te *textproto.Error
	if !errors.As(err, &te) {
		return httpx.Classify(err)
	}
	var out *engine.Error
	switch {
	case te.Code >= 400 && te.Code < 500:
		out = engine.Transient(domain.ErrorCodeNetwork, err)
	case te.Code == 530 || te.Code == 550:
		out = engine.LinkExpired(err)
	default:
		out = engine.Fatal(domain.ErrorCodeFatal, err)
	}
	out.StatusCode = te.Code
	return out
}

// unsupported reports whether the server answered that it does not implement
// the command (500, 501, 502, 504).
func unsupported(err error) bool {
	var te *textproto.Error
	if !errors.As(err, &te) {
		return false
	}
	switch te.Code {
	case 500, 501, 502, 504:
		return true
	default:
		return false
	}
}

func fatalf(format string, args ...any) error {
	return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf(format, args...))
}
