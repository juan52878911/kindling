package mysqlpw

import "testing"

// Vectores de PASSWORD() de MySQL 5.x / MariaDB (el mismo cálculo que guarda
// mysql_native_password): los conocidos de la documentación y de cualquier
// instalación.
func TestNativeHash(t *testing.T) {
	for pw, want := range map[string]string{
		"password": "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19",
		"root":     "*81F5E21E35407D884A6CD4A731AEBFB6AF209E1B",
	} {
		if got := NativeHash(pw); got != want {
			t.Errorf("NativeHash(%q) = %s, quería %s", pw, got, want)
		}
		if !ValidHash(want) {
			t.Errorf("ValidHash(%s) = false", want)
		}
	}
	for _, malo := range []string{"", "*2470c0c06dee42fd1618bb99005adca2ec9d1e19", "2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19", "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E1'"} {
		if ValidHash(malo) {
			t.Errorf("ValidHash(%q) = true", malo)
		}
	}
}
