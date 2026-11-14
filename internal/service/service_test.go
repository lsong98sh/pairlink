package service

import (
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	if got, want := Name("relay"), "pairlink-relay"; got != want {
		t.Errorf("Name(relay) = %q, want %q", got, want)
	}
	if got, want := Name(" AGENT "), "pairlink-agent"; got != want {
		t.Errorf("Name 应裁剪空白并小写: %q", got)
	}
	if got, want := Name("client"), "pairlink-client"; got != want {
		t.Errorf("Name(client) = %q, want %q", got, want)
	}
}

func TestStateString(t *testing.T) {
	tests := map[State]string{
		StateUnknown:      "unknown",
		StateStopped:      "stopped",
		StateStartPending: "start-pending",
		StateStopPending:  "stop-pending",
		StateRunning:      "running",
		StatePaused:       "paused",
	}
	for state, want := range tests {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}

func TestLogPath(t *testing.T) {
	path, err := LogPath("pairlink-relay")
	if err != nil {
		t.Fatalf("LogPath 失败: %v", err)
	}
	if !strings.HasSuffix(path, "pairlink-relay.log") {
		t.Errorf("LogPath = %q，应以 pairlink-relay.log 结尾", path)
	}
}
