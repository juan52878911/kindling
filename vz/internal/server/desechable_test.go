package server

import "testing"

// kling_disposable llega a la VM por PUT /drives o por PATCH /drives antes de
// crearla, y después de crearla se rechaza: los modos del disco se eligen al
// crear la VM y cambiarlo luego no haría nada.
func TestDiscoDesechable(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	r.configure(dir)
	r.must("PUT", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"`+dir+`/overlay.ext4","is_read_only":false,"kling_disposable":true}`)
	r.must("PATCH", "/drives/layer", `{"drive_id":"layer","path_on_host":"/i/y.layer.ext4","kling_disposable":false}`)
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	_, s, _ := r.f.last()
	for _, d := range s.Drives {
		if want := d.DriveID == "overlay"; d.Disposable != want {
			t.Fatalf("drive %s: disposable %v", d.DriveID, d.Disposable)
		}
	}
	r.mustFail("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"`+dir+`/o2.ext4","kling_disposable":false}`, "kling_disposable")
	// Solo la ruta sigue valiendo con la VM creada (Commit).
	r.must("PATCH", "/drives/overlay", `{"drive_id":"overlay","path_on_host":"`+dir+`/o2.ext4"}`)
}
