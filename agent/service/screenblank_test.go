package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func encode(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func filled(c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// A display Windows has powered down captures as uniform black.
func TestBlankScreenIsRecognised(t *testing.T) {
	if !screenIsBlank(encode(t, filled(color.Black))) {
		t.Error("a uniformly black thumbnail was not reported as blank")
	}
}

// A dark desktop is NOT a blank display, and the difference is the whole point:
// one means nothing is being drawn, the other means an operator is looking at a
// working session. A brightness tolerance here would conflate them.
func TestADarkDesktopIsNotBlank(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.Black)
		}
	}
	// One lit pixel — a cursor, a taskbar edge, anything at all.
	img.Set(10, 10, color.RGBA{R: 8, G: 8, B: 8, A: 255})
	if screenIsBlank(encode(t, img)) {
		t.Error("a nearly-black desktop with any content was reported as blank")
	}
}

// Absent is not blank, and undecodable is not blank. Both are UNKNOWN, and
// asserting a blank display from a failure to read one is the defect this whole
// field exists to avoid.
func TestUnknownIsNotReportedAsBlank(t *testing.T) {
	if screenIsBlank(nil) {
		t.Error("no thumbnail at all was reported as a blank screen")
	}
	if screenIsBlank([]byte("this is not a png")) {
		t.Error("an undecodable thumbnail was reported as a blank screen")
	}
}

// A uniform non-black frame still counts: some guests blank to another colour,
// and the claim being made is "nothing is being drawn", not "it is black".
func TestAUniformNonBlackFrameIsAlsoBlank(t *testing.T) {
	if !screenIsBlank(encode(t, filled(color.RGBA{R: 0, G: 0, B: 128, A: 255}))) {
		t.Error("a uniform non-black frame was not reported as blank")
	}
}
