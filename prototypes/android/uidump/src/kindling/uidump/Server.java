// uidump: servidor residente de UiAutomation para Android (Redroid 13).
//
// `uiautomator dump` cuesta ~1,9 s porque arranca una JVM (app_process),
// conecta UiAutomation y espera 1 s de interfaz quieta. Este servidor arranca
// la JVM una vez y mantiene UiAutomation mientras se use: cada petición es solo
// recorrer el árbol de accesibilidad.
//
// Se lanza con app_process, como uiautomator, scrcpy o uiautomator2-server sin
// APK:
//
//   CLASSPATH=/system/framework/uiautomator.jar:/system/framework/kindling-uidump.dex \
//     app_process /system/bin --nice-name=kindling-uidump kindling.uidump.Server \
//       [--release-after-ms N (30000; 0 = nunca)] [--connect] [SOCKET]
//
// El XML sale del MISMO código que `uiautomator dump`: se llama por reflexión
// al AccessibilityNodeInfoDumper de /system/framework/uiautomator.jar (su
// dumpNodeRec, con nuestro propio serializador en memoria en vez de un
// fichero), con las mismas banderas (FLAG_INCLUDE_NOT_IMPORTANT_VIEWS salvo
// --compressed), la misma raíz (getRootInActiveWindow) y la misma rotación y
// tamaño de pantalla (DisplayManagerGlobal). Si ese método no existiera, se
// usa su dumpWindowToFile con un fichero temporal.
//
// Protocolo: un socket unix de fichero (0600, root) y una petición por
// conexión, una línea de texto:
//
//   dump [--compressed] [--windows] [--idle MS] [--timeout MS]
//                                                    XML de la ventana activa (o de todas)
//   tap X Y                                          toque
//   swipe X1 Y1 X2 Y2 [MS]                           arrastre (300 ms por defecto)
//   text CADENA                                      teclea (mapa de teclado virtual)
//   key CODIGO|NOMBRE                                una tecla (3, HOME, KEYCODE_BACK...)
//   ping                                             "ok pid=... uptime_ms=..."
//   connect                                          toma UiAutomation ya (antes de guardar un dorado)
//   release                                          lo suelta (para usar uiautomator); también
//                                                    solo, tras --release-after-ms sin peticiones
//   quit                                             termina el servidor
//
// La respuesta es el cuerpo y el cierre de la conexión. Un error empieza por
// "ERROR:". Las peticiones se atienden en serie: UiAutomation no es para usarlo
// desde varios hilos a la vez.
package kindling.uidump;

import android.app.UiAutomation;
import android.graphics.Point;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.net.LocalSocketAddress;
import android.os.HandlerThread;
import android.os.Looper;
import android.os.SystemClock;
import android.system.Os;
import android.util.Xml;
import android.view.Display;
import android.view.InputDevice;
import android.view.InputEvent;
import android.view.KeyCharacterMap;
import android.view.KeyEvent;
import android.view.MotionEvent;
import android.view.accessibility.AccessibilityNodeInfo;
import android.accessibilityservice.AccessibilityServiceInfo;

import org.xmlpull.v1.XmlSerializer;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.io.StringWriter;
import java.lang.reflect.Constructor;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.TimeoutException;

public final class Server {
    static final String DEFAULT_SOCKET = "/dev/socket/kindling-uidump";
    static final String DUMPER = "com.android.uiautomator.core.AccessibilityNodeInfoDumper";

    private final String socketPath;
    private final long started = SystemClock.uptimeMillis();
    private HandlerThread thread;
    private UiAutomation ua;
    private Integer flags; // banderas nuestras ya puestas en UiAutomation, o null
    private Method dumpNodeRec; // AccessibilityNodeInfoDumper.dumpNodeRec, o null
    private Method dumpWindowToFile;
    private boolean warnedCache;
    private LocalSocket bound; // el socket de escucha; ver serve()

    // Un error que no invalida la conexión con UiAutomation.
    static final class Busy extends Exception {
        Busy(String m) { super(m); }
    }

    // UiAutomation es uno solo en todo Android: mientras lo tengamos,
    // `uiautomator dump` y las pruebas de instrumentación fallan ("already
    // registered"). Se suelta tras releaseAfterMs sin peticiones (0 = nunca) y
    // se vuelve a tomar en la siguiente (medido: ~5-8 ms en el servidor).
    private final long releaseAfterMs;
    private final boolean connectNow;
    private long lastUse = SystemClock.uptimeMillis();

    Server(String socketPath, long releaseAfterMs, boolean connectNow) {
        this.socketPath = socketPath;
        this.releaseAfterMs = releaseAfterMs;
        this.connectNow = connectNow;
    }

    // Un Timer propio y no el Looper de UiAutomation: releaseIfIdle toma el
    // cerrojo del servidor, y bloquear ese Looper mientras un dump espera
    // eventos (waitForIdle) los retrasaría.
    private void startReleaser() {
        if (releaseAfterMs <= 0) return;
        new java.util.Timer("uidump-release", true).schedule(new java.util.TimerTask() {
            @Override public void run() { releaseIfIdle(); }
        }, 1000, 1000);
    }

    private synchronized void releaseIfIdle() {
        if (ua != null && SystemClock.uptimeMillis() - lastUse >= releaseAfterMs) {
            log("idle for " + releaseAfterMs + " ms; releasing UiAutomation");
            disconnect();
        }
    }

    // Server [--release-after-ms N] [--connect] [SOCKET]
    public static void main(String[] args) throws Exception {
        String path = DEFAULT_SOCKET;
        long releaseAfter = 30000;
        boolean connectNow = false;
        for (int i = 0; i < args.length; i++) {
            if (args[i].equals("--release-after-ms") && i + 1 < args.length) releaseAfter = Long.parseLong(args[++i]);
            else if (args[i].equals("--connect")) connectNow = true;
            else path = args[i];
        }
        new Server(path, releaseAfter, connectNow).serve();
    }

    static void log(String s) { System.err.println("uidump: " + s); }

    // ── UiAutomation ─────────────────────────────────────────────────────────
    // Lo mismo que UiAutomationShellWrapper.connect() de uiautomator, por
    // reflexión: el constructor (Looper, IUiAutomationConnection) y connect()
    // son @hide. En procesos de app_process no hay lista negra de API ocultas.
    private void connect() throws Exception {
        if (ua != null) return;
        long t0 = SystemClock.uptimeMillis();
        if (thread == null) {
            thread = new HandlerThread("UiAutomatorHandlerThread");
            thread.start();
        }
        Class<?> iconn = Class.forName("android.app.IUiAutomationConnection");
        Object conn = Class.forName("android.app.UiAutomationConnection").getConstructor().newInstance();
        Constructor<UiAutomation> c = UiAutomation.class.getDeclaredConstructor(Looper.class, iconn);
        c.setAccessible(true);
        UiAutomation u = c.newInstance(thread.getLooper(), conn);
        Method m = UiAutomation.class.getDeclaredMethod("connect");
        m.setAccessible(true);
        try {
            m.invoke(u);
        } catch (java.lang.reflect.InvocationTargetException e) {
            // Solo puede haber un UiAutomation: lo tiene otro (uiautomator,
            // una prueba de instrumentación...). No es un fallo nuestro.
            if (e.getCause() instanceof IllegalStateException)
                throw new Busy("UiAutomation is held by another client (uiautomator or instrumentation running?): "
                        + e.getCause().getMessage());
            throw e;
        }
        ua = u;
        flags = null;
        log("UiAutomation connected in " + (SystemClock.uptimeMillis() - t0) + " ms");
    }

    private void disconnect() {
        if (ua == null) return;
        try {
            Method m = UiAutomation.class.getDeclaredMethod("disconnect");
            m.setAccessible(true);
            m.invoke(ua);
        } catch (Exception e) {
            log("disconnect: " + e);
        }
        ua = null;
        flags = null;
    }

    // UiAutomationShellWrapper.setCompressedLayoutHierarchy, más la bandera de
    // --windows (FLAG_RETRIEVE_INTERACTIVE_WINDOWS), y solo si cambian: cada
    // setServiceInfo es una llamada a system_server.
    private boolean setFlags(boolean comp, boolean windows) {
        int want = (comp ? 0 : AccessibilityServiceInfo.FLAG_INCLUDE_NOT_IMPORTANT_VIEWS)
                | (windows ? AccessibilityServiceInfo.FLAG_RETRIEVE_INTERACTIVE_WINDOWS : 0);
        if (flags != null && flags == want) return false;
        int mask = AccessibilityServiceInfo.FLAG_INCLUDE_NOT_IMPORTANT_VIEWS
                | AccessibilityServiceInfo.FLAG_RETRIEVE_INTERACTIVE_WINDOWS;
        AccessibilityServiceInfo info = ua.getServiceInfo();
        info.flags = (info.flags & ~mask) | want;
        ua.setServiceInfo(info);
        flags = want;
        return true;
    }

    // La caché de nodos de la conexión se mantiene con los eventos, pero se
    // vacía antes de cada dump para no devolver nunca un árbol viejo (lo que
    // haría un uiautomator recién conectado). Android 13 la tiene por conexión.
    private void clearCache() {
        try {
            Class<?> cl = Class.forName("android.view.accessibility.AccessibilityInteractionClient");
            Object client = cl.getMethod("getInstance").invoke(null);
            try {
                cl.getMethod("clearCache", int.class).invoke(client, connectionId());
                return;
            } catch (NoSuchMethodException ignored) { }
            try {
                cl.getMethod("clearCache").invoke(client);
                return;
            } catch (NoSuchMethodException ignored) { }
            if (!warnedCache) log("clearCache: no method in AccessibilityInteractionClient");
            warnedCache = true;
        } catch (Exception e) {
            log("clearCache: " + e);
        }
    }

    private int connectionId() throws Exception {
        java.lang.reflect.Field f = UiAutomation.class.getDeclaredField("mConnectionId");
        f.setAccessible(true);
        return f.getInt(ua);
    }

    // ── dump ─────────────────────────────────────────────────────────────────
    private String dump(boolean comp, boolean windows, long idleMs, long timeoutMs) throws Exception {
        connect();
        if (setFlags(comp, windows) && windows) {
            // Al activar FLAG_RETRIEVE_INTERACTIVE_WINDOWS, system_server
            // rellena la lista de ventanas en diferido (medido: la primera
            // llamada daba 0 ventanas y la segunda 1 de 3). Se espera a que
            // dejen de llegar eventos (TYPE_WINDOWS_CHANGED).
            try { ua.waitForIdle(100, 2000); } catch (TimeoutException ignored) { }
        }
        if (idleMs > 0) {
            try {
                ua.waitForIdle(idleMs, timeoutMs);
            } catch (TimeoutException e) {
                log("no idle state after " + timeoutMs + " ms; dumping anyway");
            }
        }
        if (windows) return dumpWindows();
        clearCache();
        AccessibilityNodeInfo root = ua.getRootInActiveWindow();
        if (root == null) {
            // Justo tras conectar el puente a veces aún no responde (lo dice
            // DumpCommand); un reintento corto basta.
            for (int i = 0; i < 20 && root == null; i++) {
                SystemClock.sleep(25);
                root = ua.getRootInActiveWindow();
            }
            if (root == null) throw new Busy("null root node returned by UiTestAutomationBridge (no focused window?)");
        }
        // DumpCommand de Android 13: getRealSize (con la barra de navegación),
        // no getSize.
        Display display = (Display) dmgClass().getMethod("getRealDisplay", int.class)
                .invoke(dmg(), Display.DEFAULT_DISPLAY);
        Point size = new Point();
        display.getRealSize(size);
        return serialize(root, display.getRotation(), size.x, size.y);
    }

    // dump --windows: todas las ventanas de todas las pantallas, como
    // `uiautomator dump --windows` (FLAG_RETRIEVE_INTERACTIVE_WINDOWS y
    // AccessibilityNodeInfoDumper.dumpWindowsToFile, que solo escribe a fichero).
    private String dumpWindows() throws Exception {
        clearCache();
        // Justo tras poner FLAG_RETRIEVE_INTERACTIVE_WINDOWS la lista llega
        // vacía (system_server la rellena en diferido): se espera un poco.
        android.util.SparseArray<?> windows = ua.getWindowsOnAllDisplays();
        for (int i = 0; i < 50 && windows.size() == 0; i++) {
            SystemClock.sleep(10);
            windows = ua.getWindowsOnAllDisplays();
        }
        Method m = Class.forName(DUMPER).getMethod("dumpWindowsToFile",
                android.util.SparseArray.class, File.class, dmgClass());
        File tmp = new File(socketPath + ".xml");
        m.invoke(null, windows, tmp, dmg());
        return readAndDelete(tmp);
    }

    private static Class<?> dmgClass() throws ClassNotFoundException {
        return Class.forName("android.hardware.display.DisplayManagerGlobal");
    }

    private static Object dmg() throws Exception {
        return dmgClass().getMethod("getInstance").invoke(null);
    }

    private static String readAndDelete(File f) throws IOException {
        byte[] b = new byte[(int) f.length()];
        try (FileInputStream in = new FileInputStream(f)) {
            int off = 0;
            while (off < b.length) {
                int n = in.read(b, off, b.length - off);
                if (n < 0) break;
                off += n;
            }
        }
        f.delete();
        return new String(b, StandardCharsets.UTF_8);
    }

    private String serialize(AccessibilityNodeInfo root, int rotation, int w, int h) throws Exception {
        if (dumpNodeRec == null && dumpWindowToFile == null) {
            Class<?> d = Class.forName(DUMPER);
            try {
                dumpNodeRec = d.getDeclaredMethod("dumpNodeRec", AccessibilityNodeInfo.class,
                        XmlSerializer.class, int.class, int.class, int.class);
                dumpNodeRec.setAccessible(true);
            } catch (NoSuchMethodException e) {
                dumpWindowToFile = d.getMethod("dumpWindowToFile", AccessibilityNodeInfo.class,
                        File.class, int.class, int.class, int.class);
                log("dumpNodeRec not found; falling back to dumpWindowToFile");
            }
        }
        if (dumpNodeRec != null) {
            // Idéntico a AccessibilityNodeInfoDumper.dumpWindowToFile, sin el fichero.
            StringWriter sw = new StringWriter();
            XmlSerializer s = Xml.newSerializer();
            s.setOutput(sw);
            s.startDocument("UTF-8", true);
            s.startTag("", "hierarchy");
            s.attribute("", "rotation", Integer.toString(rotation));
            dumpNodeRec.invoke(null, root, s, 0, w, h);
            s.endTag("", "hierarchy");
            s.endDocument();
            return sw.toString();
        }
        File tmp = new File(socketPath + ".xml");
        dumpWindowToFile.invoke(null, root, tmp, rotation, w, h);
        return readAndDelete(tmp);
    }

    // ── entrada ──────────────────────────────────────────────────────────────
    // Por InputManager (lo que usa `input`) y no por UiAutomation: no necesita
    // tener UiAutomation (funciona también tras `release`) y deja elegir el
    // modo. UiAutomation.injectInputEvent(ev, true) es WAIT_FOR_FINISH: espera
    // a que la app TERMINE de procesar el evento, y con render por CPU un tap
    // costaba ~1 s. WAIT_FOR_RESULT (1) espera solo a que llegue a la ventana
    // (el orden de los eventos se mantiene); luego `dump --idle MS` si hace
    // falta ver el efecto.
    static final int INJECT_ASYNC = 0;
    static final int INJECT_WAIT_FOR_RESULT = 1;
    private Object inputManager;
    private Method injectMethod;

    private void inject(InputEvent ev) throws Exception {
        inject(ev, INJECT_WAIT_FOR_RESULT);
    }

    private void inject(InputEvent ev, int mode) throws Exception {
        if (injectMethod == null) {
            Class<?> im = Class.forName("android.hardware.input.InputManager");
            inputManager = im.getMethod("getInstance").invoke(null);
            injectMethod = im.getMethod("injectInputEvent", InputEvent.class, int.class);
        }
        if (!(Boolean) injectMethod.invoke(inputManager, ev, mode))
            throw new Busy("injectInputEvent failed (event dropped by the input dispatcher)");
    }

    private void motion(long down, int action, float x, float y) throws Exception {
        MotionEvent ev = MotionEvent.obtain(down, SystemClock.uptimeMillis(), action, x, y, 0);
        ev.setSource(InputDevice.SOURCE_TOUCHSCREEN);
        try { inject(ev); } finally { ev.recycle(); }
    }

    private void tap(float x, float y) throws Exception {
        long down = SystemClock.uptimeMillis();
        motion(down, MotionEvent.ACTION_DOWN, x, y);
        motion(down, MotionEvent.ACTION_UP, x, y);
    }

    // Como `input swipe`: DOWN, MOVE interpolados hasta la duración, UP.
    private void swipe(float x1, float y1, float x2, float y2, long ms) throws Exception {
        long down = SystemClock.uptimeMillis();
        motion(down, MotionEvent.ACTION_DOWN, x1, y1);
        long end = down + ms;
        long now;
        while ((now = SystemClock.uptimeMillis()) < end) {
            float a = (float) (now - down) / ms;
            motion(down, MotionEvent.ACTION_MOVE, x1 + (x2 - x1) * a, y1 + (y2 - y1) * a);
            SystemClock.sleep(5);
        }
        motion(down, MotionEvent.ACTION_MOVE, x2, y2);
        motion(down, MotionEvent.ACTION_UP, x2, y2);
    }

    private void key(int code) throws Exception {
        long t = SystemClock.uptimeMillis();
        inject(new KeyEvent(t, t, KeyEvent.ACTION_DOWN, code, 0, 0,
                KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD));
        inject(new KeyEvent(t, SystemClock.uptimeMillis(), KeyEvent.ACTION_UP, code, 0, 0,
                KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD));
    }

    private void text(String s) throws Exception {
        KeyEvent[] evs = KeyCharacterMap.load(KeyCharacterMap.VIRTUAL_KEYBOARD).getEvents(s.toCharArray());
        if (evs == null) throw new IllegalArgumentException("text not representable with the virtual keyboard map");
        // Todas asíncronas (mismo orden: una sola cola de inyección) salvo la
        // última: esperar el resultado de cada tecla costaba ~1 s para "wifi"
        // en el buscador de Ajustes, que busca a cada pulsación.
        for (int i = 0; i < evs.length; i++)
            inject(KeyEvent.changeTimeRepeat(evs[i], SystemClock.uptimeMillis(), 0),
                    i == evs.length - 1 ? INJECT_WAIT_FOR_RESULT : INJECT_ASYNC);
    }

    private static int keyCode(String s) {
        try { return Integer.parseInt(s); } catch (NumberFormatException ignored) { }
        int c = KeyEvent.keyCodeFromString(s.startsWith("KEYCODE_") ? s : "KEYCODE_" + s);
        if (c == KeyEvent.KEYCODE_UNKNOWN) throw new IllegalArgumentException("unknown key: " + s);
        return c;
    }

    // ── servidor ─────────────────────────────────────────────────────────────
    private boolean alreadyRunning() {
        try (LocalSocket s = new LocalSocket()) {
            s.connect(new LocalSocketAddress(socketPath, LocalSocketAddress.Namespace.FILESYSTEM));
            return true;
        } catch (IOException e) {
            return false;
        }
    }

    void serve() throws Exception {
        if (alreadyRunning()) {
            log("already running at " + socketPath);
            return;
        }
        new File(socketPath).delete();
        // `bound` va en un campo: LocalServerSocket(FileDescriptor) comparte el
        // descriptor, y si el LocalSocket queda sin referencias su finalizador
        // lo cierra en el primer GC (accept daba EBADF a los pocos dumps, el
        // cliente creía muerto al servidor y arrancaba otro).
        bound = new LocalSocket();
        bound.bind(new LocalSocketAddress(socketPath, LocalSocketAddress.Namespace.FILESYSTEM));
        Os.chmod(socketPath, 0600);
        LocalServerSocket server = new LocalServerSocket(bound.getFileDescriptor());
        // Con --connect se toma UiAutomation ya (lo suelta el temporizador si
        // nadie pide nada). Sin él, en la primera petición: así el servicio
        // puede arrancar con Android sin romper `uiautomator dump`.
        if (connectNow) {
            try { connect(); } catch (Exception e) { log("connect: " + e); }
        }
        startReleaser();
        log("listening on " + socketPath + " (pid " + Os.getpid() + ")");
        int fails = 0;
        while (true) {
            LocalSocket c;
            try {
                c = server.accept();
                fails = 0;
            } catch (IOException e) {
                // Un socket roto no se arregla solo: salir y que init o el
                // cliente arranquen otro.
                log("accept: " + e);
                if (++fails >= 10) break;
                SystemClock.sleep(50);
                continue;
            }
            if (handle(c)) break;
        }
        disconnect();
        new File(socketPath).delete();
        System.exit(0);
    }

    // Devuelve true si hay que terminar.
    private synchronized boolean handle(LocalSocket c) {
        lastUse = SystemClock.uptimeMillis();
        try {
            return handle1(c);
        } finally {
            lastUse = SystemClock.uptimeMillis();
        }
    }

    private boolean handle1(LocalSocket c) {
        boolean quit = false;
        String reply;
        try {
            BufferedReader r = new BufferedReader(new InputStreamReader(c.getInputStream(), StandardCharsets.UTF_8));
            String line = r.readLine();
            if (line == null) line = "";
            try {
                String[] a = line.trim().split("\\s+");
                switch (a[0]) {
                    case "dump": {
                        boolean comp = false, windows = false;
                        long idle = 0, timeout = 10000;
                        for (int i = 1; i < a.length; i++) {
                            if (a[i].equals("--compressed")) comp = true;
                            else if (a[i].equals("--windows")) windows = true;
                            else if (a[i].equals("--idle") && i + 1 < a.length) idle = Long.parseLong(a[++i]);
                            else if (a[i].equals("--timeout") && i + 1 < a.length) timeout = Long.parseLong(a[++i]);
                            else throw new IllegalArgumentException("unknown dump option: " + a[i]);
                        }
                        reply = dump(comp, windows, idle, timeout);
                        break;
                    }
                    case "tap":
                        tap(Float.parseFloat(a[1]), Float.parseFloat(a[2]));
                        reply = "ok\n";
                        break;
                    case "swipe":
                        swipe(Float.parseFloat(a[1]), Float.parseFloat(a[2]), Float.parseFloat(a[3]),
                                Float.parseFloat(a[4]), a.length > 5 ? Long.parseLong(a[5]) : 300);
                        reply = "ok\n";
                        break;
                    case "text": {
                        String t = line.replaceFirst("^\\s*text\\s", "");
                        text(t);
                        reply = "ok\n";
                        break;
                    }
                    case "key":
                        key(keyCode(a[1]));
                        reply = "ok\n";
                        break;
                    case "ping":
                        reply = "ok pid=" + Os.getpid() + " connected=" + (ua != null)
                                + " uptime_ms=" + (SystemClock.uptimeMillis() - started) + "\n";
                        break;
                    case "connect":
                        connect();
                        reply = "connected\n";
                        break;
                    case "release":
                        disconnect();
                        reply = "released\n";
                        break;
                    case "quit":
                        quit = true;
                        reply = "bye\n";
                        break;
                    default:
                        reply = "ERROR: unknown command: " + a[0] + "\n";
                }
            } catch (Throwable t) {
                Throwable cause = t instanceof java.lang.reflect.InvocationTargetException && t.getCause() != null ? t.getCause() : t;
                reply = "ERROR: " + (cause instanceof Busy ? cause.getMessage() : cause.toString()) + "\n";
                log(line + ": " + cause);
                // Si la conexión con UiAutomation se estropeó (system_server
                // reiniciado...), la próxima petición la rehace.
                if (!(cause instanceof Busy) && (cause instanceof IllegalStateException
                        || cause instanceof android.os.DeadObjectException)) disconnect();
            }
            OutputStream out = c.getOutputStream();
            out.write(reply.getBytes(StandardCharsets.UTF_8));
            out.flush();
        } catch (IOException e) {
            log("client: " + e);
        } finally {
            try { c.close(); } catch (IOException ignored) { }
        }
        return quit;
    }
}
