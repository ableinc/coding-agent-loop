package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubCLI writes an executable that stands in for `claude`, so the whole
// runner can be exercised without spending subscription usage.
func stubCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub.sh")
	body := "#!/bin/sh\n" + script
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// successResult is a real result event captured from claude 2.1.232, trimmed.
const successResult = `{"type":"result","subtype":"success","is_error":false,"result":"Added the retry helper.",` +
	`"session_id":"03ed2543","total_cost_usd":0.0155816,"num_turns":3,"duration_ms":1723,"stop_reason":"end_turn",` +
	`"terminal_reason":"completed","api_error_status":null,` +
	`"usage":{"input_tokens":10,"output_tokens":38,"cache_creation_input_tokens":6790,"cache_read_input_tokens":12206},` +
	`"modelUsage":{"claude-opus-5-20260101":{"inputTokens":531,"outputTokens":50,"costUSD":0.0155816,` +
	`"canonicalModel":"claude-opus-5","provider":"firstParty"}}}`

func TestRunParsesResultEvent(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
echo '{"type":"system","subtype":"init","session_id":"03ed2543"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}'
echo '`+successResult+`'
`)
	logPath := filepath.Join(t.TempDir(), "run.jsonl")

	var kinds []string
	res, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, Prompt: "do the thing", LogPath: logPath,
		OnEvent: func(kind string, _ json.RawMessage) { kinds = append(kinds, kind) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Result != "Added the retry helper." || res.SessionID != "03ed2543" || res.NumTurns != 3 {
		t.Fatalf("result fields not parsed: %+v", res)
	}
	if res.TotalCostUSD != 0.0155816 {
		t.Fatalf("cost = %v", res.TotalCostUSD)
	}
	// Cache traffic must be counted, or cache-heavy runs under-report input.
	if got := res.TokensIn(); got != 10+6790+12206 {
		t.Fatalf("TokensIn() = %d, want cache traffic included", got)
	}
	if res.TokensOut() != 38 {
		t.Fatalf("TokensOut() = %d", res.TokensOut())
	}
	// The model that actually served the run, which may differ from the one
	// requested when --fallback-model kicks in.
	if got := res.PrimaryModel(); got != "claude-opus-5" {
		t.Fatalf("PrimaryModel() = %q, want the canonical id", got)
	}
	if used := res.ModelsUsed(); len(used) != 1 || used[0] != "claude-opus-5" {
		t.Fatalf("ModelsUsed() = %v", used)
	}
	if len(kinds) != 3 || kinds[0] != "system" || kinds[2] != "result" {
		t.Fatalf("stream events not surfaced in order: %v", kinds)
	}

	transcript, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("transcript missing: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(transcript)), "\n") + 1; lines != 3 {
		t.Fatalf("transcript should hold every stream line, got %d", lines)
	}
}

func TestRunSurfacesReportedErrorWithResult(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Claude AI usage limit reached|1900000000","total_cost_usd":0.002,"num_turns":1,"usage":{"input_tokens":5,"output_tokens":1}}'
`)
	res, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if err == nil {
		t.Fatal("an is_error result must be reported as an error")
	}
	// The caller needs the result to classify the failure and bill the run.
	if res == nil {
		t.Fatal("the result must still be returned alongside the error")
	}
	if res.TotalCostUSD != 0.002 {
		t.Fatalf("spend should be recorded even on failure, got %v", res.TotalCostUSD)
	}
}

func TestRunWithoutResultEvent(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
echo '{"type":"system","subtype":"init"}'
`)
	_, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("want ErrNoResult, got %v", err)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
echo "boom" >&2
exit 3
`)
	_, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr should be surfaced in the error, got %v", err)
	}
}

func TestRunSkipsNonJSONLines(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
echo 'warning: something plain and not json'
echo '`+successResult+`'
`)
	res, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if err != nil {
		t.Fatalf("a stray non-JSON diagnostic must not fail the run: %v", err)
	}
	if res.Result == "" {
		t.Fatal("result should still be parsed")
	}
}

func TestRunTimeoutIsReported(t *testing.T) {
	bin := stubCLI(t, `cat > /dev/null
sleep 5
`)
	start := time.Now()
	_, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, LogPath: filepath.Join(t.TempDir(), "run.jsonl"), Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a run past its timeout must fail")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not actually kill the process")
	}
}

func TestPromptIsDeliveredOnStdin(t *testing.T) {
	// The stub echoes back whatever it was given on stdin, proving the prompt
	// never has to survive shell quoting or argument-length limits.
	bin := stubCLI(t, `PROMPT=$(cat)
printf '{"type":"result","subtype":"success","is_error":false,"result":"%s"}\n' "$PROMPT"
`)
	res, err := (&Runner{}).Run(context.Background(), Options{
		Binary: bin, Prompt: "implement issue 42", LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "implement issue 42" {
		t.Fatalf("prompt did not reach the CLI on stdin, got %q", res.Result)
	}
}

func TestLogPathRequired(t *testing.T) {
	if _, err := (&Runner{}).Run(context.Background(), Options{Binary: "true"}); err == nil {
		t.Fatal("LogPath must be required so every run leaves a transcript")
	}
}

func TestNilResultHelpersDoNotPanic(t *testing.T) {
	var r *Result
	if r.TokensIn() != 0 || r.TokensOut() != 0 || r.PrimaryModel() != "" || r.ModelsUsed() != nil {
		t.Fatal("nil-receiver helpers should be safe")
	}
}
