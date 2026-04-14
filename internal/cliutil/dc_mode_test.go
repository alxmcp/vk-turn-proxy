package cliutil

import (
	"context"
	"errors"
	"testing"
)

func TestRunSelectedDataChannelModeSelectsProviderAndMode(t *testing.T) {
	t.Parallel()

	testErr := errors.New("selected")
	tests := []struct {
		name       string
		yalink     string
		jazzRoom   string
		vlessMode  bool
		wantLabel  string
		wantRoom   string
		wantTarget string
		wantRunner string
	}{
		{
			name:       "telemost udp",
			yalink:     "https://telemost.yandex.ru/j/room",
			wantLabel:  "Telemost",
			wantRoom:   "https://telemost.yandex.ru/j/room",
			wantTarget: "127.0.0.1:9000",
			wantRunner: "telemost",
		},
		{
			name:       "telemost vless",
			yalink:     "https://telemost.yandex.ru/j/room",
			vlessMode:  true,
			wantLabel:  "Telemost",
			wantRoom:   "https://telemost.yandex.ru/j/room",
			wantTarget: "127.0.0.1:9000",
			wantRunner: "telemost-vless",
		},
		{
			name:       "jazz udp",
			jazzRoom:   "room:password",
			wantLabel:  "SaluteJazz",
			wantRoom:   "room:password",
			wantTarget: "127.0.0.1:9000",
			wantRunner: "jazz",
		},
		{
			name:       "jazz vless",
			jazzRoom:   "room:password",
			vlessMode:  true,
			wantLabel:  "SaluteJazz",
			wantRoom:   "room:password",
			wantTarget: "127.0.0.1:9000",
			wantRunner: "jazz-vless",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			called := ""
			label, err := RunSelectedDataChannelMode(
				context.Background(),
				tt.yalink,
				tt.jazzRoom,
				"127.0.0.1:9000",
				tt.vlessMode,
				func(_ context.Context, room, target string) error {
					called = "telemost"
					if room != tt.wantRoom || target != tt.wantTarget {
						t.Fatalf("telemost runner got room=%q target=%q", room, target)
					}
					return testErr
				},
				func(_ context.Context, room, target string) error {
					called = "telemost-vless"
					if room != tt.wantRoom || target != tt.wantTarget {
						t.Fatalf("telemost vless runner got room=%q target=%q", room, target)
					}
					return testErr
				},
				func(_ context.Context, room, target string) error {
					called = "jazz"
					if room != tt.wantRoom || target != tt.wantTarget {
						t.Fatalf("jazz runner got room=%q target=%q", room, target)
					}
					return testErr
				},
				func(_ context.Context, room, target string) error {
					called = "jazz-vless"
					if room != tt.wantRoom || target != tt.wantTarget {
						t.Fatalf("jazz vless runner got room=%q target=%q", room, target)
					}
					return testErr
				},
			)
			if !errors.Is(err, testErr) {
				t.Fatalf("RunSelectedDataChannelMode() err = %v, want %v", err, testErr)
			}
			if label != tt.wantLabel {
				t.Fatalf("RunSelectedDataChannelMode() label = %q, want %q", label, tt.wantLabel)
			}
			if called != tt.wantRunner {
				t.Fatalf("called runner = %q, want %q", called, tt.wantRunner)
			}
		})
	}
}

func TestRunSelectedDataChannelModeReportsNoSelection(t *testing.T) {
	t.Parallel()

	label, err := RunSelectedDataChannelMode(
		context.Background(),
		"",
		"",
		"127.0.0.1:9000",
		false,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("RunSelectedDataChannelMode() err = %v, want nil", err)
	}
	if label != "" {
		t.Fatalf("RunSelectedDataChannelMode() label = %q, want empty", label)
	}
}
