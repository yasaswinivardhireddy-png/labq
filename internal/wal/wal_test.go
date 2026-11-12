package wal

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAppendAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.wal")
	l, recs, err := Open(path)
	if err != nil || len(recs) != 0 {
		t.Fatalf("Open = %v, %v", recs, err)
	}
	want := [][]byte{[]byte(`{"seq":1}`), []byte(`{"seq":2,"text":"a\nb"}`), []byte(``)}
	for _, r := range want {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Append([]byte("two\nlines")); err == nil {
		t.Error("a record with a raw newline was accepted")
	}
	l.Close()

	l, recs, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !slices.EqualFunc(recs, want, bytes.Equal) {
		t.Fatalf("reopened records = %q, want %q", recs, want)
	}
}

// A crash in the middle of a write leaves a torn last line, which must be dropped and cut off so
// that later appends follow the last good record.
func TestTornTailIsCutOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.wal")
	l, _, _ := Open(path)
	_ = l.Append([]byte(`{"seq":1}`))
	_ = l.Append([]byte(`{"seq":2}`))
	l.Close()
	good, _ := os.ReadFile(path)
	line3, _ := Encode([]byte(`{"seq":3}`))
	if err := os.WriteFile(path, append(slices.Clone(good), line3[:7]...), 0o600); err != nil {
		t.Fatal(err)
	}

	l, recs, err := Open(path)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records after a torn write = %q, %v; want the 2 complete ones", recs, err)
	}
	if size, _ := os.Stat(path); size.Size() != int64(len(good)) {
		t.Fatalf("file is %d bytes, want it cut back to %d", size.Size(), len(good))
	}
	_ = l.Append([]byte(`{"seq":3}`))
	l.Close()
	l, recs, _ = Open(path)
	defer l.Close()
	if len(recs) != 3 || string(recs[2]) != `{"seq":3}` {
		t.Fatalf("after appending again: %q", recs)
	}
}

func TestDamagedRecordStopsReplay(t *testing.T) {
	a, _ := Encode([]byte(`{"seq":1}`))
	b, _ := Encode([]byte(`{"seq":2}`))
	c, _ := Encode([]byte(`{"seq":3}`))
	b[12] ^= 0x01 // flip a bit inside the second record
	recs, valid := Decode(slices.Concat(a, b, c))
	if len(recs) != 1 || valid != len(a) {
		t.Fatalf("Decode = %q (%d bytes); want only the record before the damage", recs, valid)
	}
}

func FuzzDecode(f *testing.F) {
	a, _ := Encode([]byte(`{"seq":1,"type":"submit"}`))
	b, _ := Encode([]byte(`{}`))
	f.Add(slices.Concat(a, b))
	f.Add([]byte("00000000 \n"))
	f.Add([]byte("zzzzzzzz x\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		recs, valid := Decode(data)
		if valid < 0 || valid > len(data) {
			t.Fatalf("valid = %d of %d bytes", valid, len(data))
		}
		// Re-encoding what was decoded must reproduce the valid prefix exactly.
		var again []byte
		for _, r := range recs {
			line, err := Encode(r)
			if err != nil {
				t.Fatal(err)
			}
			again = append(again, line...)
		}
		if !bytes.Equal(again, data[:valid]) {
			t.Fatalf("re-encoded %q, want %q", again, data[:valid])
		}
	})
}
