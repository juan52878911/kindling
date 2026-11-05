package aigw

import (
	"strings"
	"testing"
)

// Casos reales que el filtro de antes dejaba pasar (o tapaba a medias): cada
// uno lleva el trozo que NO puede quedar en la salida.
func TestRedactTapa(t *testing.T) {
	for _, c := range []struct{ in, secreto string }{
		{"Authorization: Basic dXNlcjpodW50ZXIy", "dXNlcjpodW50ZXIy"},
		{"curl -H 'Authorization: Basic YWRtaW46czNjcjN0'", "YWRtaW46czNjcjN0"},
		{"Proxy-Authorization: Digest a1b2c3d4e5", "a1b2c3d4e5"},
		{"Authorization: Bearer abc.def.ghi", "abc.def"},
		{"set bearer Zx9YwVu8Ts7R and retry", "Zx9YwVu8Ts7R"},
		{"connect to postgres://app:Pr0dPass!@db.internal:5432/app", "Pr0dPass!"},
		{"mysql://root:hunter2@10.0.0.5/shop", "hunter2"},
		{"redis://:s3cretpw@cache:6379", "s3cretpw"},
		{"PGPASSWORD=hunter2 psql -h db", "hunter2"},
		{"export MYSQL_PWD=t0p-s3cret", "t0p-s3cret"},
		{"GET /cb?access_token=ya29abc&state=1", "ya29abc"},
		{"client_secret=GOCSPX-abcdef123", "GOCSPX-abcdef123"},
		{`{"password": "correct horse battery"}`, "correct horse"},
		{`{"api_key":"k-1234"}`, "k-1234"},
		{"DB_PASSWORD='p@ss w0rd'", "p@ss w0rd"},
		{"STRIPE_SECRET_KEY: rk_abc", "rk_abc"},
		{"my password is Hunter2! don't share", "Hunter2!"},
		{"la contraseña es Verano2026", "Verano2026"},
		{"password: hunter22 please", "hunter22"},
		{"token 3f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3", "3f9a8b"},
		{"use sk_" + "live_4eC39HqLyjWDarjtT1zdp7dc", "sk_" + "live_4eC"},
		{"rk_" + "test_51Habc123XYZ", "rk_" + "test_51H"},
		{"webhook whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZI", "whsec_" + "MfK"},
		{"GITLAB=glpat-xYz12345AbCd", "glpat-xYz"},
		{"hf_AbCdEfGhIjKlMn", "hf_AbCd"},
		{"key=AIzaSyA1b2C3d4", "AIzaSyA1"},
		{"AIza" + "SyD-9tSrke72PouQMnMX-a7eZSW0jkFMBWY", "AIza" + "SyD"},
		{"key AKIAABCDEFGHIJKLMNOP here", "AKIAABCD"},
		{"ghp_" + strings.Repeat("a1", 20), "ghp_"},
		{"sk-ant-api03-abcdefghijklmnop", "sk-ant-api03"},
		{"blob Zm9vYmFyQmF6UXV4MTIzNDU2Nzg5MGFiY2RlZg==", "Zm9vYmFy"},
		{"write to ops@example.org", "ops@"},
	} {
		out := redact(c.in)
		if strings.Contains(out, c.secreto) || !strings.Contains(out, "[redacted]") {
			t.Errorf("redact(%q) = %q (queda %q)", c.in, out, c.secreto)
		}
	}
}

// Prosa y hashes que el filtro de antes borraba de más: salen intactos.
func TestRedactNoTapa(t *testing.T) {
	for _, keep := range []string{
		"token expired, please log in again",
		"the secret sauce is butter",
		"reset your password please",
		"password reset link sent",
		"api key rotation policy",
		"the bearer of bad news",
		"basic understanding of the api",
		"tokens: 1200",
		"max_tokens=512",
		"secretary: Ana",
		"passwordless login is enabled",
		"commit 9fceb02d0ae598e95dc970b74767f19372d61af8 fixed it",
		"sha256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"request 123e4567-e89b-12d3-a456-426614174000 failed",
		"https://example.com/path?q=1",
		"open the quarterly report",
		"internationalization",
		"mueve la fila 50",
		"the token budget is 500",
		"sk-learn is a library",
		"he said the password was wrong",
	} {
		if out := redact(keep); out != keep {
			t.Errorf("redact(%q) = %q", keep, out)
		}
	}
}
