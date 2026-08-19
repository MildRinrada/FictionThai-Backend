package storage

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLocal_PutOpenDelete(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	const key = "avatar/0b7f9e2e-test.png"
	payload := []byte("fake image bytes")

	if err := store.Put(key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reader, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("Open returned %q (err %v), want %q", got, err, payload)
	}

	// Replacing an existing key succeeds atomically.
	if err := store.Put(key, strings.NewReader("v2")); err != nil {
		t.Fatalf("Put replace: %v", err)
	}

	if err := store.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Idempotent: deleting again is a success (docs/09 §33).
	if err := store.Delete(key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v, want ErrNotFound", err)
	}
}

// Keys are generated upstream, but path handling must refuse escape attempts
// regardless of the caller (docs/11 §28).
func TestLocal_RefusesTraversal(t *testing.T) {
	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	for _, key := range []string{
		"", "..", "../secret", "a/../../secret", `..\secret`, "c:/windows/system32",
	} {
		if err := store.Put(key, strings.NewReader("x")); err == nil {
			t.Errorf("Put(%q) accepted a traversal key", key)
		}
		if _, err := store.Open(key); !errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want ErrNotFound", key, err)
		}
		// Delete treats an unreachable key as already gone - never an escape.
		if err := store.Delete(key); err != nil {
			t.Errorf("Delete(%q) = %v, want nil", key, err)
		}
	}
}
