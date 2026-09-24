package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"screwlogger/internal/protocol"
)

// Buffer is a fsynced append-only JSONL file with ack-based pruning (spec §3.3).
// Entries are appended and synced before any upload is attempted; Ack advances
// the acked prefix; the file is compacted when the acked prefix is at least half.
type Buffer struct {
	path     string
	maxBytes int64
	f        *os.File
	ackedOff int64 // bytes of file covered by prior Acks
	size     int64 // total current file size in bytes
}

// OpenBuffer opens (or creates) the buffer file and recovers offsets after restart.
// O_APPEND forces every write to EOF, so an Append after a partial Unacked/Ack
// scan (which moves the fd offset mid-file) can never clobber buffered entries.
func OpenBuffer(path string, maxBytes int64) (*Buffer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Buffer{path: path, maxBytes: maxBytes, f: f}, nil
}

// Append writes one heartbeat as a JSON line and fsyncs. On overflow of unacked
// data beyond maxBytes, drops the oldest half of unacked entries (loud log).
func (b *Buffer) Append(h protocol.Heartbeat) error {
	if err := b.rotateIfFull(); err != nil {
		return err
	}
	line, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := b.f.Write(append(line, '\n')); err != nil {
		return err
	}
	b.size += int64(len(line)) + 1
	return b.f.Sync()
}

// Unacked returns up to limit oldest unacked heartbeats in file order.
func (b *Buffer) Unacked(limit int) ([]protocol.Heartbeat, error) {
	if limit <= 0 {
		return nil, nil
	}
	if _, err := b.f.Seek(b.ackedOff, 0); err != nil {
		return nil, err
	}
	var out []protocol.Heartbeat
	sc := bufio.NewScanner(b.f)
	for sc.Scan() && len(out) < limit {
		var h protocol.Heartbeat
		if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
			return nil, fmt.Errorf("corrupt buffer line: %w", err)
		}
		out = append(out, h)
	}
	return out, sc.Err()
}

// Ack marks the first count unacked entries as uploaded. When the acked prefix
// is at least half the file, the file is compacted (rewritten without acked lines).
func (b *Buffer) Ack(count int) error {
	if count <= 0 {
		return nil
	}
	if _, err := b.f.Seek(b.ackedOff, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(b.f)
	var advanced int64
	for i := 0; i < count && sc.Scan(); i++ {
		advanced += int64(len(sc.Bytes())) + 1
	}
	if err := sc.Err(); err != nil {
		return err
	}
	b.ackedOff += advanced
	if b.ackedOff*2 >= b.size {
		return b.compact()
	}
	return nil
}

// compact rewrites the file keeping only unacked entries, atomically via rewrite.
func (b *Buffer) compact() error {
	unacked, err := b.Unacked(1 << 30)
	if err != nil {
		return err
	}
	return b.rewrite(unacked)
}

// rewrite atomically replaces the buffer file with the given entries: it writes
// path+".tmp", fsyncs it, closes the old fd, renames the temp file over the
// original, and reopens the fd (with O_APPEND, so post-rewrite appends land at
// the new EOF, which is 0 after an empty rewrite). A crash at any point leaves
// either the old or the new file intact — never a truncated one.
func (b *Buffer) rewrite(entries []protocol.Heartbeat) error {
	tmp := b.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var n int64
	for _, h := range entries {
		line, err := json.Marshal(h)
		if err != nil {
			tf.Close()
			return err
		}
		if _, err := tf.Write(append(line, '\n')); err != nil {
			tf.Close()
			return err
		}
		n += int64(len(line)) + 1
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	// The old fd must be closed before the rename: Windows refuses to replace
	// a file that is still open.
	if err := b.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.path); err != nil {
		return err
	}
	f, err := os.OpenFile(b.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	b.f = f
	b.ackedOff = 0
	b.size = n
	return nil
}

// rotateIfFull drops the oldest half of unacked entries when unacked bytes exceed maxBytes.
func (b *Buffer) rotateIfFull() error {
	if b.size-b.ackedOff <= b.maxBytes {
		return nil
	}
	log.Printf("buffer overflow: unacked %d bytes > max %d; dropping oldest half", b.size-b.ackedOff, b.maxBytes)
	unacked, err := b.Unacked(1 << 30)
	if err != nil {
		return err
	}
	return b.rewrite(unacked[len(unacked)/2:])
}

// Close flushes and closes the underlying file.
func (b *Buffer) Close() error { return b.f.Close() }
