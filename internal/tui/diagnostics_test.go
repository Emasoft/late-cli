package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// DiagnosticMsg carries mid-session diagnostics (hook timeouts, hook stderr,
// dropped-progress-event notices) that used to be fmt.Fprintf(os.Stderr, ...)
// writes painting raw text over the alt-screen. They must surface as a
// WARNING toast with a 6s expiry and the standard clear tick.

func TestDiagnosticMsgShowsWarningToast(t *testing.T) {
	m := NewModel(&mockOrchestrator{}, nil, nil)
	m.SetSize(120, 30)

	before := time.Now().UnixMilli()
	updated, _ := m.Update(DiagnosticMsg{Text: "late: 44 events dropped (consumer stalled)"})
	m = updated.(Model)

	if !m.ToastWarning {
		t.Fatal("DiagnosticMsg must render as a warning toast")
	}
	if m.ToastMessage != "late: 44 events dropped (consumer stalled)" {
		t.Fatalf("ToastMessage = %q", m.ToastMessage)
	}
	// 6s expiry (± scheduling slack).
	if m.ToastExpireTime < before+5500 || m.ToastExpireTime > before+7000 {
		t.Fatalf("ToastExpireTime = %d, want ~6s after %d", m.ToastExpireTime, before)
	}
	// cmd is always non-nil after Update (present() batches a frame tick),
	// so the clear tick itself is exercised by the expiry above.
	if !strings.Contains(ansi.Strip(m.statusBarView()), "events dropped") {
		t.Fatal("toast text not rendered in the status bar")
	}
}

func TestDiagnosticMsgTruncatesLongText(t *testing.T) {
	m := NewModel(&mockOrchestrator{}, nil, nil)
	m.SetSize(40, 20)

	long := strings.Repeat("x", 200)
	updated, _ := m.Update(DiagnosticMsg{Text: long})
	m = updated.(Model)

	if got := ansi.StringWidth(m.ToastMessage); got > 40 {
		t.Fatalf("toast width = %d, want <= terminal width 40", got)
	}
	if !strings.HasSuffix(m.ToastMessage, "...") {
		t.Fatalf("truncated toast %q must end with an ellipsis", m.ToastMessage)
	}
}

func TestDiagnosticMsgEmptyTextIgnored(t *testing.T) {
	m := NewModel(&mockOrchestrator{}, nil, nil)
	m.SetSize(120, 30)

	updated, _ := m.Update(DiagnosticMsg{Text: ""})
	m = updated.(Model)

	if m.ToastMessage != "" || m.ToastWarning {
		t.Fatalf("empty diagnostic produced toast %q (warning=%v), want nothing", m.ToastMessage, m.ToastWarning)
	}
}
