package sftp

import (
	"math"
	"testing"
)

func TestNewClient_NilSSHClient(t *testing.T) {
	cli, err := NewClient(nil)
	if err == nil {
		t.Fatal("expected error when passing nil ssh.Client, got nil")
	}
	if cli != nil {
		t.Fatalf("expected nil Client, got %+v", cli)
	}
}

func TestNormalizeReadRange(t *testing.T) {
	cases := []struct {
		name                  string
		total, offset, length int64
		cap                   int64
		wantOffset, wantLen   int64
	}{
		{"whole file when length omitted", 100, 0, 0, 0, 0, 100},
		{"rest of file from offset", 100, 50, 0, 0, 50, 50},
		{"explicit length", 100, 50, 10, 0, 50, 10},
		{"negative offset clamps to start", 100, -5, 0, 0, 0, 100},
		{"offset past EOF clamps to EOF (panic regression)", 100, 50000, 0, 0, 100, 0},
		{"offset past EOF with length", 100, 50000, 42, 0, 100, 0},
		{"offset at EOF", 100, 100, 0, 0, 100, 0},
		{"negative length means rest", 100, 0, -1, 0, 0, 100},
		{"length larger than remaining clamps", 100, 90, 1000, 0, 90, 10},
		{"max int length never overflows", 100, 0, math.MaxInt64, 0, 0, 100},
		{"cap truncates", 100, 0, 0, 30, 0, 30},
		{"cap ignored when file is smaller", 100, 0, 0, 4096, 0, 100},
		{"huge sparse file with cap stays bounded", math.MaxInt64, 0, 0, 8 << 20, 0, 8 << 20},
		{"zero-size file", 0, 0, 0, 8 << 20, 0, 0},
		{"negative reported size is treated as empty", -1, 0, 0, 8 << 20, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			off, ln := normalizeReadRange(tc.total, tc.offset, tc.length, tc.cap)
			if off != tc.wantOffset || ln != tc.wantLen {
				t.Fatalf("normalizeReadRange(%d,%d,%d,%d) = (%d,%d), want (%d,%d)",
					tc.total, tc.offset, tc.length, tc.cap, off, ln, tc.wantOffset, tc.wantLen)
			}
			if ln < 0 {
				t.Fatalf("length must never be negative, got %d", ln)
			}
		})
	}
}
