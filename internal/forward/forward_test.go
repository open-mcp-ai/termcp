package forward

import (
	"errors"
	"testing"
)

func TestClose_UnknownForwardReturnsErrNotFound(t *testing.T) {
	fm := NewForwardManager()
	if err := fm.Close("no-such-forward"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
