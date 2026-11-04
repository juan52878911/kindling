package credproxy

import (
	"net/netip"
	"strings"
	"testing"
)

// ValidarPuertosDB recibe credenciales ya normalizadas (con su puerto puesto):
// solo con Postgres y MySQL juntas exige MySQL en el 3306 y ninguna Postgres ahí.
func TestValidarPuertosDB(t *testing.T) {
	pg := func(d string, p int) Credential { return Credential{Kind: KindPostgres, Domain: d, Port: p} }
	my := func(d string, p int) Credential { return Credential{Kind: KindMySQL, Domain: d, Port: p} }
	http := Credential{Kind: KindHTTP, Domain: "api.example.com", Port: MySQLDefaultPort}
	casos := []struct {
		nombre string
		creds  []Credential
		error  string // "" = vale; si no, el dominio que nombra el error
	}{
		{"ninguna", nil, ""},
		{"solo http", []Credential{http}, ""},
		{"solo postgres, incluso en el 3306", []Credential{pg("a.db", 5432), pg("b.db", MySQLDefaultPort)}, ""},
		{"solo mysql, en cualquier puerto", []Credential{my("a.db", 3307), my("b.db", 13306)}, ""},
		{"juntas y bien", []Credential{pg("pg.db", PGDefaultPort), my("my.db", MySQLDefaultPort), my("my2.db", MySQLDefaultPort), pg("pg2.db", 6543)}, ""},
		// El http en el 3306 no cuenta: el proxy HTTP lo coge por el DNAT del 80/443.
		{"juntas con un http en el 3306", []Credential{pg("pg.db", PGDefaultPort), my("my.db", MySQLDefaultPort), http}, ""},
		{"mysql fuera del 3306", []Credential{pg("pg.db", PGDefaultPort), my("my.db", 3307)}, "my.db"},
		{"postgres en el 3306", []Credential{my("my.db", MySQLDefaultPort), pg("pg.db", MySQLDefaultPort)}, "pg.db"},
		{"la segunda mysql mal", []Credential{pg("pg.db", PGDefaultPort), my("a.db", MySQLDefaultPort), my("b.db", 3310)}, "b.db"},
		// Sin normalizar, el 0 no es el 3306: quien llama pone antes el puerto.
		{"mysql sin puerto", []Credential{pg("pg.db", PGDefaultPort), my("my.db", 0)}, "my.db"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			err := ValidarPuertosDB(c.creds)
			if c.error == "" {
				if err != nil {
					t.Fatalf("rechazadas: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.error) || !strings.Contains(err.Error(), "3306") {
				t.Fatalf("err = %v, quiero un error sobre %s y el 3306", err, c.error)
			}
		})
	}
}

// Tras ValidarTipo pasa lo mismo: con las dos juntas, una MySQL que no está en el
// 3306 no se acepta ni con el resto bien.
func TestValidatePuertosDBMezclados(t *testing.T) {
	creds := []Credential{
		{Kind: KindPostgres, Env: "PGPASSWORD", Domain: "pg.db", Placeholder: PlaceholderPrefix + "pg", Secret: "s-clave-de-prueba", User: "u", Database: "app", Port: 5432},
		{Kind: KindMySQL, Env: "MYSQL_PWD", Domain: "my.db", Placeholder: PlaceholderPrefix + "my", Secret: "s-clave-de-prueba", User: "u", Database: "app", Port: 3307},
	}
	for i := range creds {
		if err := ValidarTipo(&creds[i]); err != nil {
			t.Fatalf("la credencial de prueba no vale por sí sola: %v", err)
		}
	}
	if err := ValidarPuertosDB(creds); err == nil {
		t.Fatal("MySQL en el 3307 junto a Postgres aceptada")
	}
	creds[1].Port = 0
	if err := ValidarTipo(&creds[1]); err != nil || creds[1].Port != MySQLDefaultPort {
		t.Fatalf("ValidarTipo no puso el puerto por defecto: %d, %v", creds[1].Port, err)
	}
	if err := ValidarPuertosDB(creds); err != nil {
		t.Fatalf("con el puerto por defecto: %v", err)
	}
}

func TestUpstreamProhibidos(t *testing.T) {
	lista := UpstreamProhibidos()
	if len(lista) == 0 {
		t.Fatal("ningún rango prohibido")
	}
	for _, s := range lista {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Errorf("%q no es un prefijo: %v", s, err)
			continue
		}
		if p != p.Masked() {
			t.Errorf("%q tiene bits fuera de la máscara", s)
		}
		// Cada rango que se anuncia se aplica: su primera y su última dirección.
		ultima := ultimaDe(p)
		for _, a := range []netip.Addr{p.Addr(), ultima} {
			if !UpstreamIPProhibida(a) {
				t.Errorf("%s está en %s, que se anuncia como prohibido, y se acepta", a, s)
			}
		}
	}

	// Lo que tiene que estar sí o sí: metadatos de cloud (v4, v6 y por NAT64),
	// el enlace del invitado y los veth de kindling, y su forma ::ffff:.
	for _, ip := range []string{
		"169.254.169.254", "::ffff:169.254.169.254", "fd00:ec2::254", "64:ff9b::a9fe:a9fe",
		"172.16.0.1", "172.16.0.2", "172.30.0.1", "172.30.255.254",
		"0.0.0.0", "255.255.255.255", "224.0.0.1", "::", "fe80::1", "ff02::1",
	} {
		if !UpstreamIPProhibida(netip.MustParseAddr(ip)) {
			t.Errorf("%s no está prohibida", ip)
		}
	}
	// Y lo que no: un upstream fijado por el operador puede ser su LAN, el
	// loopback (fuera del rango de reenvíos, que se mira aparte) o internet.
	for _, ip := range []string{
		"10.0.0.5", "192.168.1.10", "172.16.0.4", "172.31.0.1", "127.0.0.1", "::1",
		"8.8.8.8", "2001:db8::1", "fd00:ec2::253", "::ffff:10.0.0.5",
	} {
		if UpstreamIPProhibida(netip.MustParseAddr(ip)) {
			t.Errorf("%s está prohibida y no debería", ip)
		}
	}
	// La dirección cero (sin parsear) no se marca nunca.
	if !UpstreamIPProhibida(netip.Addr{}) {
		t.Error("una dirección inválida se acepta")
	}

	// Es una copia: cambiarla no abre nada.
	lista[0] = "203.0.113.0/24"
	if UpstreamProhibidos()[0] == "203.0.113.0/24" {
		t.Error("UpstreamProhibidos devuelve la lista interna")
	}
	if UpstreamIPProhibida(netip.MustParseAddr("203.0.113.1")) {
		t.Error("cambiar la copia cambió lo prohibido")
	}
}

// ultimaDe es la última dirección de p (todos los bits de host a 1).
func ultimaDe(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
