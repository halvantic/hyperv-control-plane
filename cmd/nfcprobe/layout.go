package main

import (
	"encoding/binary"
	"fmt"
	"strings"
)

/*
Where does the data actually start?

	A stream-optimised VMDK is read sequentially, so the reader has to know which
	sector the first grain marker sits on. Get that wrong and it lands in
	padding — and a zero-filled sector parses as a perfectly valid end-of-stream
	marker, so the decode finishes immediately, cleanly, and having copied
	nothing.

	That is exactly what the first real warm migration did: header parsed, then
	"the stream ended without a footer after 0 bytes of disk". The header's own
	descriptorOffset and descriptorSize were used to skip past the descriptor,
	and the arithmetic put the reader somewhere that is not the first marker.

	Rather than reason about the specification a second time, this prints the
	header's layout fields and then says what each of the first forty sectors
	actually holds. The answer is then a reading, not a theory.
*/
func describeHeader(b []byte) {
	if len(b) < 512 {
		return
	}
	le := binary.LittleEndian
	descOff := le.Uint64(b[28:36])
	descSize := le.Uint64(b[36:44])
	fmt.Printf("header    version=%d flags=%#x capacity=%d sectors grain=%d sectors\n",
		le.Uint32(b[4:8]), le.Uint32(b[8:12]), le.Uint64(b[12:20]), le.Uint64(b[20:28]))
	fmt.Printf("layout    descriptorOffset=%d descriptorSize=%d numGTEsPerGT=%d overHead=%d rgdOffset=%d gdOffset=%d\n",
		descOff, descSize, le.Uint32(b[44:48]), le.Uint64(b[64:72]), le.Uint64(b[48:56]), le.Uint64(b[56:64]))
	fmt.Printf("          Ballast currently starts reading markers at sector %d\n", 1+(descOff-1)+descSize)

	n := len(b) / 512
	if n > 40 {
		n = 40
	}
	fmt.Printf("sectors   what each of the first %d sectors actually holds:\n", n)
	for i := 0; i < n; i++ {
		fmt.Printf("  %3d  %s\n", i, classifySector(b[i*512:(i+1)*512]))
	}
}

// classifySector names one 512-byte sector the way the decoder would read it.
func classifySector(s []byte) string {
	if isZero(s) {
		return "all zeros — WOULD BE READ AS AN END-OF-STREAM MARKER"
	}
	if string(s[:4]) == "KDMV" {
		return "the sparse header"
	}
	if strings.HasPrefix(string(s), "# Disk Descriptor") {
		return "descriptor text: " + strings.TrimSpace(strings.SplitN(string(s), "\n", 2)[0])
	}
	le := binary.LittleEndian
	val, size := le.Uint64(s[0:8]), le.Uint32(s[8:12])
	if size != 0 {
		return fmt.Sprintf("GRAIN MARKER — lba=%d (disk offset %d) compressed=%d bytes", val, val*512, size)
	}
	switch le.Uint32(s[12:16]) {
	case 0:
		return "metadata marker: END OF STREAM"
	case 1:
		return fmt.Sprintf("metadata marker: grain table, %d sectors follow", val)
	case 2:
		return fmt.Sprintf("metadata marker: grain directory, %d sectors follow", val)
	case 3:
		return fmt.Sprintf("metadata marker: footer, %d sectors follow", val)
	}
	return fmt.Sprintf("unrecognised (val=%d size=%d type=%d)", val, size, le.Uint32(s[12:16]))
}

func isZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
