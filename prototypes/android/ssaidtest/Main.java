package kindling.ssaidtest;

import android.app.Activity;
import android.os.Bundle;
import android.provider.Settings;
import android.util.Log;
import android.widget.TextView;
import java.io.File;
import java.io.FileOutputStream;

/**
 * Cada vez que pasa a primer plano (onResume, no solo al crearse: tras un
 * freeze/thaw el proceso sigue vivo y `am start` no lo recrea) escribe el ANDROID_ID (SSAID) que ve esta app en logcat, con la etiqueta
 * SSAIDTEST y una línea "SSAIDTEST <paquete> <android_id>", y también en
 * files/ssaid.txt. Sin permisos ni dependencias.
 */
public class Main extends Activity {
    @Override protected void onResume() {
        super.onResume();
        String id = Settings.Secure.getString(getContentResolver(), Settings.Secure.ANDROID_ID);
        String linea = getPackageName() + " " + id;
        Log.i("SSAIDTEST", linea);
        try (FileOutputStream o = new FileOutputStream(new File(getFilesDir(), "ssaid.txt"))) {
            o.write((linea + "\n").getBytes());
        } catch (Exception e) {
            Log.w("SSAIDTEST", "no se pudo escribir ssaid.txt: " + e);
        }
        TextView t = new TextView(this);
        t.setText(linea);
        setContentView(t);
    }
}
