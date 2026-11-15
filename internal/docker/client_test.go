package docker

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hel"))
	buf.Write(frame(2, "err1\n"))
	buf.Write(frame(1, "lo\nworld"))
	var out, errs []string
	if err := Demux(&buf, func(s string) { out = append(out, s) }, func(s string) { errs = append(errs, s) }); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != "hello" || out[1] != "world" || len(errs) != 1 {
		t.Fatalf("out=%q errs=%q", out, errs)
	}
}
