package main

import (
	"bytes"
	"image"
	_ "image/png"
)

// screenIsBlank reports whether a console thumbnail is a single flat colour.
//
// Windows powers a console display down on its inactivity timer, and the
// framebuffer then captures as uniform black. Rendered as-is that is a black
// rectangle indistinguishable from a working session showing a dark desktop —
// so an operator sees what looks like a live console and concludes the VM is
// broken. It is worth one image decode per capture to be able to say which.
//
// WHAT THIS DOES NOT ESTABLISH: whether anyone is signed in. A blanked display
// looks identical whether the session beneath it is active, locked, or sitting at
// the logon screen. Only waking it distinguishes those, and that means sending
// input into someone's guest — an operator's decision, never a background one.
// So this reports a blank display and stops there.
func screenIsBlank(png []byte) bool {
	if len(png) == 0 {
		return false
	}
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		// Undecodable is UNKNOWN, not blank. Reporting a blank screen because the
		// picture could not be read would be an assertion built on a failure.
		return false
	}
	b := img.Bounds()
	if b.Empty() {
		return false
	}
	r0, g0, b0, _ := img.At(b.Min.X, b.Min.Y).RGBA()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, _ := img.At(x, y).RGBA()
			// Exact equality on purpose. A blanked display is uniform; a real
			// desktop that merely looks dark is not, and a tolerance here would
			// start calling dark wallpapers blank.
			if r != r0 || g != g0 || bb != b0 {
				return false
			}
		}
	}
	return true
}
