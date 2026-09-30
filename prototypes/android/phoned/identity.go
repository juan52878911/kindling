package main

// La identidad de cada clon (issue #92). Llega por MMDS tras restaurar, como
// secreto de sesión (`kling machine secret <m> -hooks`, stdin):
//
//	{"phone": {
//	   "android_id": "<16 hex>",           obligatorio
//	   "name":       "<nombre visible>",   opcional
//	   "serial":     "<ro.serialno>",      opcional; si falta, uno al azar
//	   "adb_keys":   ["<clave pública de adb>", ...],   opcional
//	   "ssaid":      "regen" | "keep"      opcional; regen por defecto
//	   "ssaid_key":  "<64 hex>"            opcional; la clave de SSAID de este
//	                                       teléfono (para conservar los ANDROID_ID
//	                                       por app de uno que se rehace); si
//	                                       falta, una al azar
//	   "api_tokens": [{"sha256": "<64 hex>", "scope": "read"|"control"}]
//	                                       opcional; los tokens que abre la API
//	                                       del 8091 (auth.go). Solo sus sha256:
//	                                       el token no pasa por MMDS
//	}}
//
// Un documento con api_tokens y SIN android_id solo cambia los tokens (rotar
// o revocar sin rehacer la identidad): {"phone": {"api_tokens": []}} cierra la
// API.
//
// y el gancho (`kling-phoned identity`) la aplica dentro de Android. Nada de
// esto se escribe en un log ni en la línea de órdenes de un proceso: los
// valores van por la entrada estándar de la shell de Android o directamente
// a la memoria de propiedades. La salida del gancho (que acaba en la consola
// del invitado, `kling logs`) solo dice QUÉ se aplicó.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type phoneIdentity struct {
	AndroidID string   `json:"android_id"`
	Name      string   `json:"name,omitempty"`
	Serial    string   `json:"serial,omitempty"`
	AdbKeys   []string `json:"adb_keys,omitempty"`
	SSAID     string   `json:"ssaid,omitempty"`
	SSAIDKey  string   `json:"ssaid_key,omitempty"`
	// APITokens: nil = no se tocan; vacío = ninguno (API cerrada).
	APITokens *[]apiToken `json:"api_tokens,omitempty"`

	sum string // huella del documento recibido (digest)
}

type mmdsDoc struct {
	Phone *phoneIdentity `json:"phone"`
}

var (
	reAndroidID = regexp.MustCompile(`^[0-9a-f]{16}$`)
	reName      = regexp.MustCompile(`^[A-Za-z0-9._-]{0,40}$`)
	reSerial    = regexp.MustCompile(`^[A-Za-z0-9]{6,20}$`)
	reKeyB64    = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)
	reKeyNote   = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
	reSSAIDKey  = regexp.MustCompile(`^[0-9A-Fa-f]{64}$`)
)

// adbPubKeyLen es el tamaño de la clave RSA de adb decodificada
// (adb/crypto/rsa_2048_key.cpp: len, n0inv, n[64], rr[64], exponente).
const adbPubKeyLen = 4 + 4 + 256 + 256 + 4

const maxAdbKeys = 8

// validate comprueba el documento y rellena lo que falta (serie al azar,
// ssaid=regen). Los mensajes de error nunca incluyen el valor.
func (p *phoneIdentity) validate() error {
	if p.APITokens != nil {
		if err := validateTokens(*p.APITokens); err != nil {
			return err
		}
	}
	if p.authOnly() {
		return nil
	}
	if !reAndroidID.MatchString(p.AndroidID) {
		return fmt.Errorf("phone.android_id must be 16 lowercase hex digits")
	}
	if !reName.MatchString(p.Name) {
		return fmt.Errorf("phone.name must match [A-Za-z0-9._-]{0,40}")
	}
	if p.Serial == "" {
		s, err := randomSerial()
		if err != nil {
			return err
		}
		p.Serial = s
	}
	if !reSerial.MatchString(p.Serial) {
		return fmt.Errorf("phone.serial must be 6-20 letters or digits")
	}
	if len(p.AdbKeys) > maxAdbKeys {
		return fmt.Errorf("phone.adb_keys: at most %d keys", maxAdbKeys)
	}
	for i, k := range p.AdbKeys {
		if err := checkAdbKey(k); err != nil {
			return fmt.Errorf("phone.adb_keys[%d]: %v", i, err)
		}
	}
	switch p.SSAID {
	case "":
		p.SSAID = "regen"
	case "regen", "keep":
	default:
		return fmt.Errorf("phone.ssaid must be regen or keep")
	}
	if p.SSAIDKey == "" {
		var k [32]byte
		if _, err := rand.Read(k[:]); err != nil {
			return err
		}
		p.SSAIDKey = hex.EncodeToString(k[:])
	}
	if !reSSAIDKey.MatchString(p.SSAIDKey) {
		return fmt.Errorf("phone.ssaid_key must be 64 hex digits")
	}
	// Como la escribe SettingsProvider (generateUserKeyLocked): mayúsculas.
	p.SSAIDKey = strings.ToUpper(p.SSAIDKey)
	return nil
}

// authOnly: el documento solo trae tokens (ni android_id ni nada más).
func (p *phoneIdentity) authOnly() bool {
	return p.APITokens != nil && p.AndroidID == "" && p.Name == "" && p.Serial == "" &&
		len(p.AdbKeys) == 0 && p.SSAID == "" && p.SSAIDKey == ""
}

// ssaidXML es settings_ssaid.xml con solo la clave de usuario, en el formato
// de texto de SettingsState (el lector acepta texto y ABX): de ella salen, al
// pedirlos, los ANDROID_ID de cada app (HMAC-SHA256 con la firma de la app).
func (p *phoneIdentity) ssaidXML() string {
	return "<?xml version='1.0' encoding='utf-8' standalone='yes' ?>\n<settings version=\"-1\">\n" +
		`<setting id="0" name="userkey" value="` + p.SSAIDKey + `" package="android" defaultValue="` +
		p.SSAIDKey + `" defaultSysSet="true" />` + "\n</settings>\n"
}

// checkAdbKey: "<base64 de 524 bytes>[ <comentario>]", como una línea de
// ~/.android/adbkey.pub.
func checkAdbKey(k string) error {
	b64, note, _ := strings.Cut(strings.TrimSpace(k), " ")
	if !reKeyB64.MatchString(b64) {
		return fmt.Errorf("not an adb public key (base64 expected)")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != adbPubKeyLen {
		return fmt.Errorf("not an adb public key (%d bytes decoded, want %d)", len(raw), adbPubKeyLen)
	}
	if note != "" && !reKeyNote.MatchString(note) {
		return fmt.Errorf("bad comment after the key")
	}
	return nil
}

// adbKeysFile es el contenido de /data/misc/adb/adb_keys.
func (p *phoneIdentity) adbKeysFile() string {
	var b strings.Builder
	for _, k := range p.AdbKeys {
		b.WriteString(strings.TrimSpace(k))
		b.WriteByte('\n')
	}
	return b.String()
}

func randomSerial() (string, error) {
	const alfabeto = "ABCDEFGHJKLMNPQRSTUVWXYZ0123456789"
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alfabeto[int(b[i])%len(alfabeto)]
	}
	return string(b[:]), nil
}

// fetchPhoneIdentity lee el almacén de MMDS (v2: token por PUT y lectura con
// él; el mismo flujo que pkg/guest). (nil, nil) = no hay identidad.
func fetchPhoneIdentity(ctx context.Context, base string) (*phoneIdentity, error) {
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-metadata-token-ttl-seconds", "30")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mmds token: %w", err)
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(tok) == 0 {
		return nil, fmt.Errorf("mmds token: HTTP %d", resp.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-metadata-token", strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	resp, err = cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mmds store: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mmds store: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("mmds store: %w", err)
	}
	return parseMMDS(body)
}

func parseMMDS(body []byte) (*phoneIdentity, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	var d mmdsDoc
	if err := json.Unmarshal(body, &d); err != nil {
		// Sin el valor: el cuerpo es el secreto.
		return nil, fmt.Errorf("mmds store is not the expected JSON document")
	}
	if d.Phone == nil {
		return nil, nil
	}
	// La huella, del documento tal cual llegó: validate rellena lo que falta
	// al azar y el mismo documento tiene que dar la misma huella.
	d.Phone.sum = d.Phone.digest()
	if err := d.Phone.validate(); err != nil {
		return nil, err
	}
	return d.Phone, nil
}

// parseSSAID lee settings_ssaid.xml (ya en texto): la clave de usuario
// (userkey, de la que salen todos los SSAID) se devuelve solo como huella; los
// SSAID por paquete son identificadores, no secretos.
func parseSSAID(b []byte) (string, map[string]string) {
	var doc struct {
		Settings []struct {
			Name    string `xml:"name,attr"`
			Value   string `xml:"value,attr"`
			Package string `xml:"package,attr"`
		} `xml:"setting"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return "", nil
	}
	key := ""
	pk := map[string]string{}
	for _, s := range doc.Settings {
		if s.Name == "userkey" {
			h := sha256.Sum256([]byte(s.Value))
			key = hex.EncodeToString(h[:])[:16]
			continue
		}
		if s.Package != "" && s.Value != "" {
			pk[s.Package] = s.Value
		}
	}
	return key, pk
}

// digest identifica un documento de identidad sin guardarlo (marca de
// "ya aplicada"). Tras parseMMDS es la del documento recibido. Los tokens no
// cuentan: cambiarlos no rehace la identidad.
func (p *phoneIdentity) digest() string {
	if p.sum != "" {
		return p.sum
	}
	c := *p
	c.APITokens = nil
	b, _ := json.Marshal(&c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
