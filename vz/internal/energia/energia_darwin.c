// Avisos de sueño del Mac (IORegisterForSystemPower), ver energia_darwin.go.

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOMessage.h>
#include <IOKit/pwr_mgt/IOPMLib.h>

extern void energiaEvento(int tipo, long id);

static io_connect_t raiz = MACH_PORT_NULL;

static void aviso(void *refcon, io_service_t servicio, natural_t tipo, void *arg) {
	switch (tipo) {
	case kIOMessageCanSystemSleep:
		// Un sueño por inactividad pregunta antes: no se veta nunca.
		IOAllowPowerChange(raiz, (long)arg);
		break;
	case kIOMessageSystemWillSleep:
		// Se contesta después, desde Go (energiaPermitir), con las VM ya
		// pausadas o al vencer el plazo.
		energiaEvento(1, (long)arg);
		break;
	case kIOMessageSystemHasPoweredOn:
		energiaEvento(2, 0);
		break;
	}
}

int energiaRegistrar(void) {
	IONotificationPortRef puerto;
	io_object_t notificador;
	raiz = IORegisterForSystemPower(NULL, &puerto, aviso, &notificador);
	if (raiz == MACH_PORT_NULL) {
		return -1;
	}
	CFRunLoopAddSource(CFRunLoopGetCurrent(), IONotificationPortGetRunLoopSource(puerto), kCFRunLoopDefaultMode);
	return 0;
}

void energiaCorrer(void) { CFRunLoopRun(); }

void energiaPermitir(long id) { IOAllowPowerChange(raiz, id); }
