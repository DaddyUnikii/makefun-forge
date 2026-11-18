package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
)

// writePack builds a pack version 2 of plain (non-delta) objects.
func writePack(objs []*GitObj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 2)
	buf.Write(hdr[:])
	binary.BigEndian.PutUint32(hdr[:], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		var code uint32
		switch o.Type {
		case "commit":
			code = 1
		case "tree":
			code = 2
		case "blob":
			code = 3
		case "tag":
			code = 4
		}
		size := len(o.Data)
		first := byte(code<<4) | byte(size&0x0f)
		rem := size >> 4
		var hb []byte
		hb = append(hb, first)
		for rem > 0 {
			hb[len(hb)-1] |= 0x80
			hb = append(hb, byte(rem&0x7f))
			rem >>= 7
		}
		buf.Write(hb)
		zw := zlib.NewWriter(&buf)
		zw.Write(o.Data)
		zw.Close()
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	buf.Write(sum[:])
	return buf.Bytes()
}

// countingReader counts bytes read from r.
type countingReader struct {
	r *bytes.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// ReadByte makes countingReader a flate.Reader (io.ByteReader), so compress/zlib
// uses it directly instead of wrapping it in a bufio.Reader that reads ahead —
// without this, c.n over-reports and the zlib stream boundary is wrong.
func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// inflateFrom inflates the zlib stream starting at data[start:], returning the
// raw payload and the offset just past the compressed stream.
func inflateFrom(data []byte, start int) ([]byte, int, error) {
	cr := &countingReader{r: bytes.NewReader(data[start:])}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, start, err
	}
	raw, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, start, err
	}
	return raw, start + cr.n, nil
}

type rawEntry struct {
	typ      string
	data     []byte
	offset   int
	deltaOfs int
	deltaRef string
}

// readPack parses a received pack (version 2/3), resolving delta entries.
// Base resolution order is handled explicitly: plain entries resolve first,
// then ofs-delta (which references earlier plain entries), then ref-delta in a
// fixed-point loop (a legal REF base can appear later in the pack).
func readPack(data []byte) (map[string]*GitObj, error) {
	if len(data) < 32 || string(data[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	trailer := data[len(data)-20:]
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(trailer, sum[:]) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	body := data[12 : len(data)-20]

	entries := make([]rawEntry, 0, count)
	off := 0
	for i := 0; i < count; i++ {
		if off >= len(body) {
			return nil, fmt.Errorf("pack truncated")
		}
		entryOffset := off
		b := body[off]
		off++
		typCode := (b >> 4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= len(body) {
				return nil, fmt.Errorf("pack size truncated")
			}
			b = body[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		typ := ""
		deltaOfs := -1
		var deltaRef string
		switch typCode {
		case 1:
			typ = "commit"
		case 2:
			typ = "tree"
		case 3:
			typ = "blob"
		case 4:
			typ = "tag"
		case 6:
			typ = "ofs-delta"
			b = body[off]
			off++
			val := int(b & 0x7f)
			for b&0x80 != 0 {
				b = body[off]
				off++
				val = ((val + 1) << 7) | int(b&0x7f)
			}
			deltaOfs = val
		case 7:
			typ = "ref-delta"
			if off+20 > len(body) {
				return nil, fmt.Errorf("ref-delta truncated")
			}
			deltaRef = hexEncode(body[off : off+20])
			off += 20
		default:
			return nil, fmt.Errorf("bad pack type %d", typCode)
		}
		raw, next, err := inflateFrom(body, off)
		if err != nil {
			return nil, fmt.Errorf("inflate at %d: %v", off, err)
		}
		off = next
		entries = append(entries, rawEntry{typ: typ, data: raw, offset: entryOffset, deltaOfs: deltaOfs, deltaRef: deltaRef})
	}

	type resolvedEntry struct {
		typ   string
		data  []byte
		oid   string
		done  bool
		isObj bool
	}
	resolved := make([]resolvedEntry, len(entries))
	out := make(map[string]*GitObj, count)

	// Pass 1: plain objects.
	for i := range entries {
		e := entries[i]
		switch e.typ {
		case "commit", "tree", "blob", "tag":
			oid := hashObject(e.typ, e.data)
			resolved[i] = resolvedEntry{typ: e.typ, data: e.data, oid: oid, done: true, isObj: true}
			out[oid] = &GitObj{Type: e.typ, Data: e.data}
		}
	}

	// Pass 2..N: deltas, resolved by type and base availability via index inspection.
	// The client's writer emits plain objects, so ofs-delta bases are always plain
	// entries earlier in the pack. Handle both variants with a fixed-point loop.
	changed := true
	for guard := 0; guard < count+2 && changed; guard++ {
		changed = false
		for i := range entries {
			if resolved[i].done {
				continue
			}
			e := entries[i]
			switch e.typ {
			case "ofs-delta":
				baseIdx := -1
				for j := range entries {
					if entries[j].offset == e.offset-e.deltaOfs {
						baseIdx = j
						break
					}
				}
				if baseIdx < 0 || !resolved[baseIdx].done {
					continue
				}
				data, err := applyDelta(resolved[baseIdx].data, e.data)
				if err != nil {
					return nil, err
				}
				typ := resolved[baseIdx].typ
				oid := hashObject(typ, data)
				resolved[i] = resolvedEntry{typ: typ, data: data, oid: oid, done: true, isObj: true}
				out[oid] = &GitObj{Type: typ, Data: data}
				changed = true
			case "ref-delta":
				base, ok := out[e.deltaRef]
				if !ok {
					continue
				}
				data, err := applyDelta(base.Data, e.data)
				if err != nil {
					return nil, err
				}
				typ := base.Type
				oid := hashObject(typ, data)
				resolved[i] = resolvedEntry{typ: typ, data: data, oid: oid, done: true, isObj: true}
				out[oid] = &GitObj{Type: typ, Data: data}
				changed = true
			}
		}
	}
	for i := range entries {
		if !resolved[i].done {
			return nil, fmt.Errorf("unresolvable delta entry %d", i)
		}
	}
	return out, nil
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0xf]
	}
	return string(out)
}

// applyDelta applies a git delta to base, returning the reconstructed bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	b := base
	d := delta
	off := 0
	readSize := func() (int, error) {
		var v int
		shift := uint(0)
		for {
			if off >= len(d) {
				return 0, fmt.Errorf("delta truncated")
			}
			c := d[off]
			off++
			v |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(b) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(d) {
		op := d[off]
		off++
		if op&0x80 != 0 {
			var copyOff, copySize int
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(d) {
						return nil, fmt.Errorf("delta copy truncated")
					}
					copyOff |= int(d[off]) << (bit * 8)
					off++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if op&(1<<(bit+4)) != 0 {
					if off >= len(d) {
						return nil, fmt.Errorf("delta copy size truncated")
					}
					copySize |= int(d[off]) << (bit * 8)
					off++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(b) {
				return nil, fmt.Errorf("delta copy out of base")
			}
			copy(out[written:], b[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			lit := int(op)
			if off+lit > len(d) {
				return nil, fmt.Errorf("delta insert truncated")
			}
			copy(out[written:], d[off:off+lit])
			off += lit
			written += lit
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if written != tgtSize {
		return nil, fmt.Errorf("delta size mismatch (%d != %d)", written, tgtSize)
	}
	return out, nil
}
