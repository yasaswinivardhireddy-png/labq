// Package wal is a write-ahead log: an append-only file of records, one per line. A line is the
// record's CRC-32C as 8 hex digits, a space, and the record itself (JSON, which never contains a
// raw newline). Every append is flushed to disk before it returns.
//
// A crash can leave a torn or damaged last line. Open keeps every record before the first bad
// line and truncates the file there, so the log always ends with a complete record.
package wal

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

var table = crc32.MakeTable(crc32.Castagnoli)

// Log is an open write-ahead log.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

// Open opens or creates the log at path and returns the records it already holds.
func Open(path string) (*Log, [][]byte, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	recs, valid := Decode(data)
	if valid < len(data) {
		if err := f.Truncate(int64(valid)); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("wal: cutting a damaged tail: %w", err)
		}
	}
	if _, err := f.Seek(int64(valid), io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return &Log{f: f}, recs, nil
}

// Encode frames one record as a log line.
func Encode(rec []byte) ([]byte, error) {
	if bytes.IndexByte(rec, '\n') >= 0 {
		return nil, errors.New("wal: a record cannot contain a newline")
	}
	line := make([]byte, 0, len(rec)+10)
	line = fmt.Appendf(line, "%08x ", crc32.Checksum(rec, table))
	line = append(line, rec...)
	return append(line, '\n'), nil
}

// Decode returns the records at the start of data that are complete and intact, and how many
// bytes they take. Decoding stops at the first torn or damaged line.
func Decode(data []byte) (recs [][]byte, valid int) {
	for valid < len(data) {
		n := bytes.IndexByte(data[valid:], '\n')
		if n < 0 {
			break
		}
		line := data[valid : valid+n]
		if len(line) < 9 || line[8] != ' ' {
			break
		}
		sum, ok := parseHex32(line[:8])
		if !ok || crc32.Checksum(line[9:], table) != sum {
			break
		}
		recs = append(recs, bytes.Clone(line[9:]))
		valid += n + 1
	}
	return recs, valid
}

// parseHex32 parses exactly 8 lowercase hex digits, the only form Encode writes.
func parseHex32(b []byte) (uint32, bool) {
	var v uint32
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// Append writes a record and flushes it to disk.
func (l *Log) Append(rec []byte) error {
	line, err := Encode(rec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(line); err != nil {
		return err
	}
	return l.f.Sync()
}

// Close closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
