package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appconfig "late/internal/config"
)

// TestShouldExitOnConfigLoadError guards the strict-config startup decision:
// a load error that produced NO config is fatal (process exit before the
// TUI — R2/R3: never silently continue on fallback defaults), while an error
// that still produced a usable config (the post-load permission-hardening
// failure) only warns.
func TestShouldExitOnConfigLoadError(t *testing.T) {
	tests := []struct {
		name string
		cfg  *appconfig.Config
		err  error
		want bool
	}{
		{
			name: "clean load does not exit",
			cfg:  &appconfig.Config{},
			err:  nil,
			want: false,
		},
		{
			name: "nil config and nil error does not exit",
			cfg:  nil,
			err:  nil,
			want: false,
		},
		{
			name: "fatal: parse error with nil config exits",
			cfg:  nil,
			err: &appconfig.ConfigParseError{
				Path: "/Users/u/config.json",
				Msg:  `"compaction_mode" is not a valid config.json entry`,
			},
			want: true,
		},
		{
			name: "fatal: read error with nil config exits",
			cfg:  nil,
			err:  errors.New("failed to read /Users/u/config.json: is a directory"),
			want: true,
		},
		{
			name: "recoverable: permission error still returns the config",
			cfg:  &appconfig.Config{},
			err:  errors.New("failed to set config file permissions: chmod denied"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldExitOnConfigLoadError(tt.cfg, tt.err); got != tt.want {
				t.Fatalf("shouldExitOnConfigLoadError(%#v, %v) = %v, want %v", tt.cfg, tt.err, got, tt.want)
			}
		})
	}
}

// TestFlagOrder_EarlyFlagsWorkWithBrokenConfig pins the main() flag order:
// -version and -help are handled BEFORE appconfig.LoadConfig, so they work
// even when config.json is broken, while a plain startup with a broken
// config must print the rendered error and exit 1 (strict config: no
// fallback-to-defaults startup).
//
// The child-process pattern re-executes the test binary with an env hook
// that runs main() against a temp config dir, so os.Exit behavior is
// observable without killing the test process.
func TestFlagOrder_EarlyFlagsWorkWithBrokenConfig(t *testing.T) {
	if os.Getenv("LATE_TEST_RUN_MAIN") == "1" {
		// Child mode: rebuild the flag line and run the real main().
		args := []string{"late-test-binary"}
		if extra := os.Getenv("LATE_TEST_MAIN_ARGS"); extra != "" {
			args = append(args, strings.Split(extra, " ")...)
		}
		os.Args = args
		main()
		return
	}

	configRoot := t.TempDir()
	// The broken config must sit where os.UserConfigDir() resolves for the
	// CHILD's environment (HOME/APPDATA point at configRoot):
	//   darwin: $HOME/Library/Application Support/late/config.json
	//           (XDG_CONFIG_HOME is ignored there)
	//   linux:  $XDG_CONFIG_HOME/late/config.json
	//   windows: %APPDATA%\late\config.json
	// Writing to both layouts keeps the test platform-proof. If the file
	// were missing instead of broken, the child would take the fresh-install
	// path and START THE TUI, so both locations really must carry it.
	brokenContent := "{\n  \"compaction_mode\": true\n}\n"
	brokenPaths := []string{
		filepath.Join(configRoot, "late", "config.json"),
		filepath.Join(configRoot, "Library", "Application Support", "late", "config.json"),
	}
	for _, broken := range brokenPaths {
		if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(broken, []byte(brokenContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	// runMain executes one child run. The context timeout guarantees a
	// misbehaving child (e.g. one that reaches the TUI) fails the test
	// instead of hanging the suite.
	runMain := func(t *testing.T, args []string) (stdout, stderr string, exitCode int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestFlagOrder_EarlyFlagsWorkWithBrokenConfig$", "-test.timeout=90s")
		cmd.Env = append(os.Environ(),
			"LATE_TEST_RUN_MAIN=1",
			"LATE_TEST_MAIN_ARGS="+strings.Join(args, " "),
			// Point every config-dir resolution at the broken-config root
			// (mirrors setUserConfigEnv: XDG is ignored on darwin, HOME and
			// APPDATA are honored everywhere).
			"XDG_CONFIG_HOME="+configRoot,
			"APPDATA="+configRoot,
			"HOME="+configRoot,
		)
		var outBuf, errBuf strings.Builder
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		runErr := cmd.Run()
		exitCode = 0
		if runErr != nil {
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("running %v: %v (stderr: %s)", args, runErr, errBuf.String())
			}
			exitCode = exitErr.ExitCode()
		}
		return outBuf.String(), errBuf.String(), exitCode
	}

	t.Run("-version works with a broken config", func(t *testing.T) {
		stdout, stderr, code := runMain(t, []string{"-version"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
		}
		if !strings.HasPrefix(stdout, "late ") {
			t.Fatalf("stdout = %q, want the version banner", stdout)
		}
	})

	t.Run("-help works with a broken config", func(t *testing.T) {
		_, stderr, code := runMain(t, []string{"-help"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
		}
		if !strings.Contains(stderr, "Usage") || !strings.Contains(stderr, "-use-tools") {
			t.Fatalf("stderr = %q, want the flag help text", stderr)
		}
	})

	t.Run("broken config exits 1 with the rendered error before the TUI", func(t *testing.T) {
		_, stderr, code := runMain(t, nil)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr)
		}
		for _, part := range []string{
			`"compaction_mode" is not a valid config.json entry`,
			`Did you mean "compaction-mode"?`,
			"at line 2",
		} {
			if !strings.Contains(stderr, part) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, part)
			}
		}
		// The rendered error names the exact broken config.json the child saw.
		sawPath := false
		for _, broken := range brokenPaths {
			if strings.Contains(stderr, broken) {
				sawPath = true
				break
			}
		}
		if !sawPath {
			t.Fatalf("stderr = %q, want the exact config path", stderr)
		}
	})

	// -replay-shadow deliberately runs BEFORE LoadConfig (documented in
	// main.go: a read-only diagnostic must not take LoadConfig's side
	// effects, so it never reads config.json). A broken config therefore
	// cannot block the replay: it must print its report (in the harness's
	// fresh HOME there is no shadow log, so the nothing-scored note) and
	// exit 0 without any config error on stderr.
	t.Run("-replay-shadow ignores a broken config", func(t *testing.T) {
		stdout, stderr, code := runMain(t, []string{"-replay-shadow=0.10,0.35"})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stdout: %q, stderr: %q)", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "No shadow log at") {
			t.Fatalf("stdout = %q, want the nothing-scored note", stdout)
		}
		if strings.Contains(stderr, "not a valid config.json entry") {
			t.Fatalf("stderr = %q, want no config error on the replay path", stderr)
		}
	})

	// -check-compaction resolves the config like a normal run (the check
	// must vet exactly the backend the session would use), so a broken
	// config exits 1 with the rendered error BEFORE the preflight starts —
	// the same strict-config exit as a plain startup, never a misleading
	// "no backend" report caused by the config being unreadable.
	t.Run("-check-compaction exits 1 on a broken config before the preflight", func(t *testing.T) {
		stdout, stderr, code := runMain(t, []string{"-check-compaction"})
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr)
		}
		if !strings.Contains(stderr, `"compaction_mode" is not a valid config.json entry`) {
			t.Fatalf("stderr = %q, want the rendered config error", stderr)
		}
		// The preflight's own report must never print: the process exited
		// before runCompactionCheck could run.
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("stdout = %q, want no preflight report", stdout)
		}
	})
}
