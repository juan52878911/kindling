//go:build linux

// fbmirror copia fotogramas RGB888 crudos de stdin al framebuffer del invitado
// (/dev/fb0, la emulación fbdev de virtio-gpu). Es el puente entre Android,
// que en Redroid compone en una pantalla virtual sin KMS (hwcomposer.redroid),
// y la pantalla virtio-gpu que kling-vz enseña en el Mac:
//
//	android-sh screenrecord --output-format=raw-frames --size 720x1280 - | fbmirror -size 720x1280
//
// Cada fotograma son ANCHO*ALTO*3 bytes (R, G, B). El framebuffer es
// XRGB8888 (en memoria B, G, R, X); si mide distinto se centra o se recorta,
// sin escalar. Escribe con pwrite sobre /dev/fb0: la emulación fbdev de DRM
// marca lo escrito como sucio y lo transfiere al anfitrión ella sola.
//
// Al acabar imprime cuántos fotogramas copió y a cuántos por segundo.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// fb_var_screeninfo y fb_fix_screeninfo de <linux/fb.h> (64 bits).
type fbBitfield struct{ Offset, Length, MsbRight uint32 }

type fbVarScreeninfo struct {
	Xres, Yres, XresVirtual, YresVirtual, Xoffset, Yoffset, BitsPerPixel, Grayscale uint32
	Red, Green, Blue, Transp                                                        fbBitfield
	Nonstd, Activate, Height, Width, AccelFlags, Pixclock, LeftMargin, RightMargin  uint32
	UpperMargin, LowerMargin, HsyncLen, VsyncLen, Sync, Vmode, Rotate, Colorspace   uint32
	Reserved                                                                        [4]uint32
}

type fbFixScreeninfo struct {
	ID           [16]byte
	SmemStart    uint64
	SmemLen      uint32
	Type         uint32
	TypeAux      uint32
	Visual       uint32
	Xpanstep     uint16
	Ypanstep     uint16
	Ywrapstep    uint16
	_            uint16
	LineLength   uint32
	MmioStart    uint64
	MmioLen      uint32
	Accel        uint32
	Capabilities uint16
	Reserved     [2]uint16
	_            uint32
}

const (
	fbioGetVscreeninfo = 0x4600
	fbioPutVscreeninfo = 0x4601
	fbioGetFscreeninfo = 0x4602
)

func ioctl(fd uintptr, req uintptr, p unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(p)); e != 0 {
		return e
	}
	return nil
}

func main() {
	dev := flag.String("fb", "/dev/fb0", "framebuffer device")
	size := flag.String("size", "", "WIDTHxHEIGHT of the incoming RGB888 frames (required)")
	flag.Parse()
	var w, h int
	if _, err := fmt.Sscanf(*size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		fmt.Fprintln(os.Stderr, "fbmirror: -size WIDTHxHEIGHT is required")
		os.Exit(2)
	}
	f, err := os.OpenFile(*dev, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fbmirror:", err)
		os.Exit(1)
	}
	var vi fbVarScreeninfo
	var fi fbFixScreeninfo
	if err := ioctl(f.Fd(), fbioGetVscreeninfo, unsafe.Pointer(&vi)); err != nil {
		fmt.Fprintln(os.Stderr, "fbmirror: FBIOGET_VSCREENINFO:", err)
		os.Exit(1)
	}
	if vi.BitsPerPixel != 32 {
		fmt.Fprintf(os.Stderr, "fbmirror: %d bpp framebuffer, only 32 is supported\n", vi.BitsPerPixel)
		os.Exit(1)
	}
	// Reponer la misma configuración fuerza el modeset (drm_fb_helper_set_par):
	// sin consola en el framebuffer nadie más lo haría y el scanout seguiría
	// en negro.
	vi.Activate = 0x80 // FB_ACTIVATE_FORCE
	if err := ioctl(f.Fd(), fbioPutVscreeninfo, unsafe.Pointer(&vi)); err != nil {
		fmt.Fprintln(os.Stderr, "fbmirror: FBIOPUT_VSCREENINFO (ignored):", err)
	}
	if err := ioctl(f.Fd(), fbioGetFscreeninfo, unsafe.Pointer(&fi)); err != nil {
		fmt.Fprintln(os.Stderr, "fbmirror: FBIOGET_FSCREENINFO:", err)
		os.Exit(1)
	}
	fw, fh, stride := int(vi.Xres), int(vi.Yres), int(fi.LineLength)
	fmt.Fprintf(os.Stderr, "fbmirror: %s %dx%d stride %d, frames %dx%d\n", *dev, fw, fh, stride, w, h)

	// Centrado/recorte: qué parte de la fuente va a qué parte del destino.
	cw, ch := min(w, fw), min(h, fh)
	sx, sy := (w-cw)/2, (h-ch)/2
	dx, dy := (fw-cw)/2, (fh-ch)/2

	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	frame := make([]byte, w*h*3)
	out := make([]byte, stride*fh)
	n, t0 := 0, time.Now()
	for {
		if _, err := io.ReadFull(in, frame); err != nil {
			break
		}
		for y := 0; y < ch; y++ {
			src := frame[((sy+y)*w+sx)*3:]
			dst := out[(dy+y)*stride+dx*4:]
			for x := 0; x < cw; x++ {
				dst[x*4+0] = src[x*3+2]
				dst[x*4+1] = src[x*3+1]
				dst[x*4+2] = src[x*3+0]
				dst[x*4+3] = 0xff
			}
		}
		if _, err := f.WriteAt(out, 0); err != nil {
			fmt.Fprintln(os.Stderr, "fbmirror: write:", err)
			os.Exit(1)
		}
		n++
	}
	el := time.Since(t0).Seconds()
	fmt.Fprintf(os.Stderr, "fbmirror: %d frames in %.1f s (%.1f fps)\n", n, el, float64(n)/max(el, 0.001))
}
