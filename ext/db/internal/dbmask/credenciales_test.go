package dbmask

import (
	"errors"
	"testing"
)

// Nombres de columnas de credenciales que una base real tiene: sin regla, el
// clon se para en todos (antes la mayoría pasaba tal cual al dorado).
func TestDeteccionCredenciales(t *testing.T) {
	for _, col := range []string{
		"password", "password_hash", "encrypted_password", "passwordHash", "hashed_password",
		"user_password", "passwd", "pwd", "pass_digest", "password_enc", "passphrase",
		"api_key", "apiKey", "apikey", "api_key_digest", "access_token", "accessToken",
		"refresh_token", "session_token", "reset_token", "resetPasswordToken", "token",
		"token_hash", "client_secret", "clientSecret", "otp_secret", "totp_seed", "mfa_seed",
		"webhook_secret", "private_key", "privateKey", "stripe_secret_key", "secret_key",
		"jwt", "salt", "credentials", "auth_token_enc",
	} {
		if got := Suspicious(col); got != "credential" {
			t.Errorf("Suspicious(%q) = %q, want credential", col, got)
		}
	}
	// Controles: ni credencial ni, salvo lo que ya se marcaba, sospechosa.
	for col, want := range map[string]string{
		"table_name": "name", "primary_key": "", "sort_key": "", "key": "", "keyboard": "",
		"tokens_used": "", "passenger_count": "", "bypass": "", "compass": "", "salary": "",
		"secretary_id": "", "monkey": "", "turnkey": "", "id": "", "created_at": "",
	} {
		if Credential(col) {
			t.Errorf("Credential(%q) = true, want false", col)
		}
		if got := Suspicious(col); got != want {
			t.Errorf("Suspicious(%q) = %q, want %q", col, got, want)
		}
	}
}

// Una columna de credencial sin regla para la construcción aunque no sea -strict.
func TestPlanCredencialSinRegla(t *testing.T) {
	cat := &Catalog{
		Tables:  []Table{{Schema: "public", Name: "users", Rows: 1}},
		Columns: []Column{{Schema: "public", Table: "users", Column: "encrypted_password", Type: "text", Category: "S"}},
	}
	if _, err := NewPlan(cat, mustRules(t, `{}`), Options{}); !errors.Is(err, ErrUnmasked) {
		t.Fatalf("NewPlan = %v, want ErrUnmasked", err)
	}
}
