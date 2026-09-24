package agent

import (
	"bufio"
	"bytes"
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
func OpenBuffer(path string, maxBytes int64) (*Buffer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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

// compact rewrites the file keeping only unacked entries.
func (b *Buffer) compact() error {
	unacked, err := b.Unacked(1 << 30)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, h := range unacked {
		line, _ := json.Marshal(h)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := b.f.Truncate(0); err != nil {
		return err
	}
	if _, err := b.f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := b.f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := b.f.Sync(); err != nil {
		return err
	}
	b.ackedOff = 0
	b.size = int64(buf.Len())
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
	keep := unacked[len(unacked)/2:]
	var buf bytes.Buffer
	for _, h := range keep {
		line, _ := json.Marshal(h)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := b.f.Truncate(0); err != nil {
		return err
	}
	if _, err := b.f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := b.f.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := b.f.Sync(); err != nil {
		return err
	}
	b.ackedOff = 0
	b.size = int64(buf.Len())
	return nil
}

// Close flushes and closes the underlying file.
func (b *Buffer) Close() error { return b.f.Close() }
