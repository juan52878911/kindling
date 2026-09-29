//go:build darwin

package vzvm

/*
#cgo darwin CFLAGS: -mmacosx-version-min=13 -x objective-c -fno-objc-arc
#cgo darwin LDFLAGS: -framework Foundation -framework Cocoa -framework Virtualization
#include <stdlib.h>
#import <Cocoa/Cocoa.h>
#import <Virtualization/Virtualization.h>

// Cerrar la ventana NO para la máquina: la minimiza. La máquina es del
// daemon (kling stop/pause/save), no de la ventana.
@interface KVZWindowDelegate : NSObject <NSWindowDelegate>
@end
@implementation KVZWindowDelegate
- (BOOL)windowShouldClose:(NSWindow *)w
{
    [w miniaturize:nil];
    return NO;
}
@end

static void kvzRunApp(void)
{
    @autoreleasepool {
        [NSApplication sharedApplication];
        // Accessory: ventanas sin icono en el Dock ni menú propio; kling-vz
        // sigue siendo un proceso auxiliar.
        [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
        [NSApp run];
    }
}

static void *kvzShowVM(void *vm, double w, double h, const char *title)
{
    NSString *t = [[NSString alloc] initWithUTF8String:title];
    __block VZVirtualMachineView *view = nil;
    dispatch_sync(dispatch_get_main_queue(), ^{
        view = [[VZVirtualMachineView alloc] init];
        view.virtualMachine = (VZVirtualMachine *)vm;
        view.capturesSystemKeys = NO;
        if (@available(macOS 14.0, *)) {
            // Que el tamaño de la ventana no cambie la resolución del
            // invitado: Android tiene la suya fija.
            view.automaticallyReconfiguresDisplay = NO;
        }
        CGFloat scale = [[NSScreen mainScreen] backingScaleFactor];
        if (scale < 1) scale = 1;
        NSRect r = NSMakeRect(0, 0, w / scale, h / scale);
        NSWindow *win = [[NSWindow alloc]
            initWithContentRect:r
                      styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
                                NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
                        backing:NSBackingStoreBuffered
                          defer:NO];
        static KVZWindowDelegate *del;
        if (del == nil) del = [[KVZWindowDelegate alloc] init];
        win.delegate = del;
        win.releasedWhenClosed = NO;
        win.contentView = view;
        win.contentAspectRatio = NSMakeSize(w, h);
        win.title = t;
        [win center];
        [win makeKeyAndOrderFront:nil];
    });
    [t release];
    return view; // retenida: vive lo que el proceso
}

// kvzCapture pinta la vista en un mapa de bits y devuelve un PNG (malloc; lo
// libera quien llama). NULL si no se pudo.
static void *kvzCapture(void *viewp, size_t *len)
{
    __block NSData *png = nil;
    dispatch_sync(dispatch_get_main_queue(), ^{
        NSView *v = (NSView *)viewp;
        NSBitmapImageRep *rep = [v bitmapImageRepForCachingDisplayInRect:v.bounds];
        if (rep == nil) return;
        [v cacheDisplayInRect:v.bounds toBitmapImageRep:rep];
        png = [[rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}] retain];
    });
    if (png == nil) return NULL;
    *len = png.length;
    void *buf = malloc(*len);
    memcpy(buf, png.bytes, *len);
    [png release];
    return buf;
}
*/
import "C"

import (
	"errors"
	"os"
	"reflect"
	"runtime"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
)

// WindowMode dice si este proceso enseña la pantalla de la VM en una ventana
// (KLING_VZ_WINDOW=1). Solo sirve con una máquina que tenga pantalla
// (KLING_VZ_GRAPHICS, o un snapshot guardado con ella).
func WindowMode() bool { return os.Getenv("KLING_VZ_WINDOW") == "1" }

func init() {
	// AppKit exige el hilo principal. Fijar la gorrutina main a él tiene que
	// hacerse en init, antes de que el planificador la mueva.
	if WindowMode() {
		runtime.LockOSThread()
	}
}

// RunApp corre el bucle de AppKit en el hilo principal. No vuelve: quien la
// llame debe hacer el trabajo en otras gorrutinas y salir con os.Exit.
func RunApp() { C.kvzRunApp() }

// showWindow abre una ventana con la pantalla de la VM y devuelve su vista
// (nil si no se pudo).
func showWindow(vm *vz.VirtualMachine, w, h int, title string) unsafe.Pointer {
	p := objcPtr(vm)
	if p == nil {
		return nil
	}
	ct := C.CString(title)
	defer C.free(unsafe.Pointer(ct))
	v := C.kvzShowVM(p, C.double(w), C.double(h), ct)
	runtime.KeepAlive(vm)
	return v
}

// capture devuelve la vista pintada como PNG.
func capture(view unsafe.Pointer) ([]byte, error) {
	if view == nil {
		return nil, errors.New("this VM has no window (KLING_VZ_WINDOW=1 and a screen are needed)")
	}
	var n C.size_t
	buf := C.kvzCapture(view, &n)
	if buf == nil {
		return nil, errors.New("could not capture the window")
	}
	defer C.free(buf)
	return C.GoBytes(buf, C.int(n)), nil
}

// objcPtr saca el VZVirtualMachine* de un *vz.VirtualMachine. Code-Hex/vz no
// lo exporta (lo guarda en el campo embebido "pointer", un *objc.Pointer cuyo
// primer campo es el puntero). Si una versión nueva cambia la forma, se
// devuelve nil y no hay ventana en vez de un fallo de memoria.
func objcPtr(vm *vz.VirtualMachine) unsafe.Pointer {
	f := reflect.ValueOf(vm).Elem().FieldByName("pointer")
	if !f.IsValid() || f.Kind() != reflect.Pointer || f.IsNil() {
		return nil
	}
	if f.Type().Elem().Kind() != reflect.Struct || f.Type().Elem().NumField() < 1 ||
		f.Type().Elem().Field(0).Type.Kind() != reflect.UnsafePointer {
		return nil
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(f.Pointer()))
}
