package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAcceptsTheDocumentedValuesAndEmpty(t *testing.T) {
	for _, lvl := range []string{"", "debug", "info", "warn", "error"} {
		for _, f := range []string{"", "text", "json"} {
			if err := Validate(lvl, f); err != nil {
				t.Errorf("Validate(%q, %q) = %v, want nil", lvl, f, err)
			}
		}
	}
}

// TestValidateRejectsEverythingElse: a typo'd level must fail config load,
// not quietly log at info while the operator believes debug is on.
func TestValidateRejectsEverythingElse(t *testing.T) {
	for _, c := range []struct{ level, format, want string }{
		{"verbose", "text", "log_level"},
		{"INFO", "text", "log_level"},
		{"warning", "text", "log_level"},
		{"info", "logfmt", "log_format"},
		{"info", "JSON", "log_format"},
	} {
		err := Validate(c.level, c.format)
		if err == nil {
			t.Errorf("Validate(%q, %q) = nil, want an error", c.level, c.format)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Validate(%q, %q) = %v, want it to name %s", c.level, c.format, err, c.want)
		}
	}
}

func TestNewHonorsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New("warn", "text", &buf)
	l.Info("dropped")
	l.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "dropped") || !strings.Contains(out, "kept") {
		t.Errorf("level warn: got %q", out)
	}

	buf.Reset()
	New("debug", "text", &buf).Debug("debugline")
	if !strings.Contains(buf.String(), "debugline") {
		t.Errorf("level debug dropped a debug line: %q", buf.String())
	}

	buf.Reset()
	New("error", "text", &buf).Warn("warnline")
	if buf.Len() != 0 {
		t.Errorf("level error kept a warn line: %q", buf.String())
	}
}

// TestNewDefaultsToInfoText: the zero values are what a config that names
// neither key gets, so they must be the documented defaults.
func TestNewDefaultsToInfoText(t *testing.T) {
	var buf bytes.Buffer
	l := New("", "", &buf)
	l.Debug("hidden")
	l.Info("shown", "k", "v")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("default level logged debug: %q", out)
	}
	if !strings.Contains(out, "msg=shown") || !strings.Contains(out, "k=v") {
		t.Errorf("default format is not text: %q", out)
	}
}

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	New("info", "json", &buf).Info("hello", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("json format did not write JSON: %v (%q)", err, buf.String())
	}
	if m["msg"] != "hello" || m["k"] != "v" || m["level"] != "INFO" {
		t.Errorf("json record = %v", m)
	}
}
