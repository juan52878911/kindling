// Avisos de sueño del Mac (IORegisterForSystemPower) y de la fuente de
// alimentación (IOPSCreateLimitedPowerNotification), ver energia_darwin.go.

#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOMessage.h>
#include <IOKit/ps/IOPSKeys.h>
#include <IOKit/ps/IOPowerSources.h>
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

// energiaFuente dice de dónde come el Mac: 1 batería, 0 otra cosa (corriente,
// SAI), -1 no se sabe.
int energiaFuente(void) {
	CFTypeRef foto = IOPSCopyPowerSourcesInfo();
	if (foto == NULL) {
		return -1;
	}
	int v = -1;
	CFStringRef tipo = IOPSGetProvidingPowerSourceType(foto);
	if (tipo != NULL) {
		v = CFStringCompare(tipo, CFSTR(kIOPMBatteryPowerKey), 0) == kCFCompareEqualTo;
	}
	CFRelease(foto);
	return v;
}

// cambioFuente: el paso de corriente a batería o al revés (el aviso
// «limitado», no el de cada punto de porcentaje).
static void cambioFuente(void *contexto) { energiaEvento(3, energiaFuente()); }

int energiaRegistrar(void) {
	IONotificationPortRef puerto;
	io_object_t notificador;
	raiz = IORegisterForSystemPower(NULL, &puerto, aviso, &notificador);
	if (raiz == MACH_PORT_NULL) {
		return -1;
	}
	CFRunLoopAddSource(CFRunLoopGetCurrent(), IONotificationPortGetRunLoopSource(puerto), kCFRunLoopDefaultMode);
	// Sin el aviso de la fuente el vigía sigue valiendo para el sueño: el
	// daemon, sin noticias de la fuente, la sigue preguntando él.
	CFRunLoopSourceRef fuente = IOPSCreateLimitedPowerNotification(cambioFuente, NULL);
	if (fuente != NULL) {
		CFRunLoopAddSource(CFRunLoopGetCurrent(), fuente, kCFRunLoopDefaultMode);
		CFRelease(fuente);
	}
	return 0;
}

void energiaCorrer(void) { CFRunLoopRun(); }

void energiaPermitir(long id) { IOAllowPowerChange(raiz, id); }
