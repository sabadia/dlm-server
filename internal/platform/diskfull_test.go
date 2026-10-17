package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
)

func TestIsDiskFull(t *testing.T) {
	type tc struct {
		name string
		err  error
		want bool
	}
	tests := []tc{
		{"nil", nil, false},
		{"other", errors.New("boom"), false},
		{"not exist", fs.ErrNotExist, false},
	}
	for _, e := range diskFullErrnos() {
		tests = append(tests,
			tc{fmt.Sprintf("errno %d in PathError", uintptr(e)), &os.PathError{Op: "write", Path: "f", Err: e}, true},
			tc{fmt.Sprintf("errno %d wrapped", uintptr(e)), fmt.Errorf("x: %w", e), true},
		)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDiskFull(tt.err); got != tt.want {
				t.Errorf("IsDiskFull(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
