package kindling.itest;

import android.app.Instrumentation;
import android.os.Bundle;
import android.view.accessibility.AccessibilityNodeInfo;

public class T extends Instrumentation {
    @Override public void onCreate(Bundle b) { super.onCreate(b); start(); }

    @Override public void onStart() {
        Bundle res = new Bundle();
        try {
            AccessibilityNodeInfo r = getUiAutomation().getRootInActiveWindow();
            res.putString("stream", "ITEST OK root=" + (r != null ? r.getPackageName() : "null") + "\n");
        } catch (Throwable t) {
            res.putString("stream", "ITEST FAIL " + t + "\n");
        }
        finish(0, res);
    }
}
