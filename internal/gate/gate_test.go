package gate

import (
	"net/http"
	"testing"
	"time"

	"github.com/ableinc/coding-agent-loop/internal/claude"
)

func TestDetectLimit(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).Unix()
	status429 := http.StatusTooManyRequests
	status500 := http.StatusInternalServerError

	tests := []struct {
		name        string
		result      *claude.Result
		runErr      error
		wantLimited bool
		wantReset   bool
	}{
		{
			name:        "no signal at all",
			result:      &claude.Result{},
			wantLimited: false,
		},
		{
			name:        "ordinary failure is not a limit",
			result:      &claude.Result{IsError: true, Result: "file not found: main.go"},
			runErr:      errString("claude exited with error: exit status 1"),
			wantLimited: false,
		},
		{
			name:        "explicit 429 from the api",
			result:      &claude.Result{APIErrorStatus: &status429},
			wantLimited: true,
		},
		{
			name:        "500 is not a usage limit",
			result:      &claude.Result{APIErrorStatus: &status500, Result: "internal error"},
			wantLimited: false,
		},
		{
			name:        "limit phrase with epoch reset",
			result:      &claude.Result{IsError: true, Result: "Claude AI usage limit reached|" + itoa(reset)},
			wantLimited: true,
			wantReset:   true,
		},
		{
			name:        "limit phrase without a reset time",
			result:      &claude.Result{IsError: true, Result: "You've reached your usage limit for this period."},
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "limit reported on stderr",
			result:      &claude.Result{Stderr: "error: rate limit exceeded, try again later"},
			wantLimited: true,
		},
		{
			name:        "limit only visible in the run error",
			runErr:      errString("claude exited: too many requests"),
			wantLimited: true,
		},
		{
			name:        "stale reset timestamp is ignored",
			result:      &claude.Result{IsError: true, Result: "usage limit reached|1000000000"},
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "absurdly distant reset timestamp is ignored",
			result:      &claude.Result{IsError: true, Result: "usage limit reached|" + itoa(time.Now().Add(72*time.Hour).Unix())},
			wantLimited: true,
			wantReset:   false,
		},
		{
			name:        "nil result with nil error",
			wantLimited: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hit, limited := DetectLimit(tc.result, tc.runErr)
			if limited != tc.wantLimited {
				t.Fatalf("limited = %v, want %v (reason %q)", limited, tc.wantLimited, hit.Reason)
			}
			if !limited {
				return
			}
			if hit.HasReset != tc.wantReset {
				t.Fatalf("HasReset = %v, want %v", hit.HasReset, tc.wantReset)
			}
			if tc.wantReset && !hit.ResetAt.After(time.Now()) {
				t.Fatalf("ResetAt %v should be in the future", hit.ResetAt)
			}
			if hit.Reason == "" {
				t.Fatal("a detected limit must carry a reason for /status")
			}
		})
	}
}

func TestMinorToUnits(t *testing.T) {
	two := 2
	zero := 0
	tests := []struct {
		amount float64
		exp    *int
		want   float64
	}{
		{36370, &two, 363.70},
		{36370, nil, 363.70}, // exponent defaults to 2
		{750, &zero, 750},
	}
	for _, tc := range tests {
		if got := minorToUnits(tc.amount, tc.exp); got != tc.want {
			t.Errorf("minorToUnits(%v) = %v, want %v", tc.amount, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
